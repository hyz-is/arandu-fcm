package fcm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/arandu-io/framework/data"
	"github.com/arandu-io/framework/security"
	"github.com/arandu-io/framework/validation"
	"github.com/arandu-io/hesape/notifications"
)

const (
	// minTokenBytes and maxTokenBytes bound a registration token. Firebase's are
	// about 160 characters; the bounds only refuse what cannot be one.
	minTokenBytes = 8
	maxTokenBytes = 4096
	// maxAppBytes bounds the application id.
	maxAppBytes = 200
	// maxListed bounds the devices one read answers with.
	maxListed = 100
	// maxNotified bounds the devices one notification fans out to.
	maxNotified = 100
	// cleanBatch is how many stale devices one pass of the clean removes before
	// it reads again.
	cleanBatch = 200
)

var (
	appPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]*$`)
	localePattern = regexp.MustCompile(`^[A-Za-z]{2,3}([-_][A-Za-z0-9]{2,8})*$`)
)

// DeviceService orchestrates the use cases of the devices: it validates the
// request, asks the policy for a Grant, applies the rules the entity holds for
// itself, and persists with that Grant.
//
// It receives its collaborators through the constructor. There is no container
// and no resolution by reflection: what this service is made of is written at
// the one place that builds it.
//
// Every exported method authorizes before it reaches the Model. The service is
// the only owner of the database handle, so the request layer cannot reach a
// Model before the policy has answered.
type DeviceService struct {
	db      *data.DB
	policy  DevicePolicy
	sealer  *sealer
	audit   *auditor
	now     func() time.Time
	maxEach int
}

// NewDeviceService wires the service over the application's database handle.
// Only Config.Key, Config.MaxDevicesPerUser and Config.Clock matter to it; the
// module passes its whole configuration and an application that uses the
// service alone passes those three.
func NewDeviceService(db *data.DB, cfg Config) (*DeviceService, error) {
	if len(cfg.Key) < MinKeyBytes {
		return nil, fmt.Errorf("fcm: Config.Key has %d bytes and needs at least %d: pass the application's key", len(cfg.Key), MinKeyBytes)
	}
	if cfg.MaxDevicesPerUser < 0 {
		return nil, errors.New("fcm: Config.MaxDevicesPerUser cannot be negative")
	}
	cfg = cfg.withDefaults()
	sealed, err := newSealer(cfg.Key)
	if err != nil {
		return nil, err
	}
	return &DeviceService{
		db:      db,
		sealer:  sealed,
		audit:   newAuditor(db, cfg.Clock),
		now:     cfg.Clock,
		maxEach: cfg.MaxDevicesPerUser,
	}, nil
}

// RegisterRequest is the input contract of Register.
//
// The fields are explicit and there is no mass assignment, so a request body
// cannot write a column nobody meant to expose. There is no tenant and no user
// here and there must never be one: the tenant comes from the Grant, and the
// device belongs to the subject who registers it.
type RegisterRequest struct {
	// Token is the registration token Firebase gave the installation.
	Token string
	// Platform is android or ios.
	Platform string
	// App is the application id (the Android package or the iOS bundle).
	App string
	// Locale is the language of the installation, a BCP 47 tag; optional.
	Locale string
}

// registration is a request that passed validation.
type registration struct {
	token    string
	platform Platform
	app      string
	locale   string
}

// Validate reports the errors per field.
func (r RegisterRequest) Validate() validation.Errors {
	_, errs := r.normalize()
	return errs
}

// Compile-time proof that the request honors the validation contract.
var _ validation.Validatable = RegisterRequest{}

func (r RegisterRequest) normalize() (registration, validation.Errors) {
	e := validation.Errors{}
	out := registration{
		token:  strings.TrimSpace(r.Token),
		app:    strings.TrimSpace(r.App),
		locale: strings.TrimSpace(r.Locale),
	}

	switch {
	case out.token == "":
		e.Add("token", "The token is required.")
	case len(out.token) < minTokenBytes || len(out.token) > maxTokenBytes:
		e.Add("token", fmt.Sprintf("The token has to have between %d and %d characters.", minTokenBytes, maxTokenBytes))
	case !printable(out.token):
		e.Add("token", "The token has characters a registration token does not have.")
	}

	platform, ok := ParsePlatform(r.Platform)
	if !ok {
		e.Add("platform", "The platform has to be android or ios.")
	}
	out.platform = platform

	if out.app != "" && (len(out.app) > maxAppBytes || !appPattern.MatchString(out.app)) {
		e.Add("app", "The app has to be an application id such as com.example.app.")
	}
	if out.locale != "" {
		if len(out.locale) > 35 || !localePattern.MatchString(out.locale) {
			e.Add("locale", "The locale has to be a language tag such as pt-BR.")
		} else {
			out.locale = strings.ReplaceAll(out.locale, "_", "-")
		}
	}
	return out, e
}

// printable reports whether a string is visible ASCII with no space.
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// Register stores the phone of the subject, or refreshes it.
//
// It is idempotent by token: registering a token the tenant already holds
// updates that row and does not add one. The token may change hands -- a phone
// passed to another user, or signed into another account -- and the same token
// then replaces its earlier registration: the row now belongs to the subject who
// presented it. A user who passes MaxDevicesPerUser loses the phone seen least
// recently.
//
// The tenant is the Grant's and the owner is the subject's; neither is read
// from the request.
func (s *DeviceService) Register(ctx context.Context, actor security.Subject, in RegisterRequest) (*Device, error) {
	valid, errs := in.normalize()
	if errs.Any() {
		return nil, errs
	}
	g, err := security.Authorize(ctx, s.policy, actor, DeviceRegister, Device{UserID: actor.ID})
	if err != nil {
		return nil, err
	}
	hash := s.sealer.hash(data.Tenant(g), valid.token)

	var out *Device
	run := func() error {
		out = nil
		return data.Transaction(ctx, s.db, func(ctx context.Context) error {
			record, err := s.register(ctx, g, actor, valid, hash)
			out = record
			return err
		})
	}
	err = run()
	if err != nil && s.holds(ctx, g, hash) {
		// A concurrent registration of the same token won the unique index
		// between the read and the write. The row exists now, so the second
		// attempt updates it.
		err = run()
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// register is the body of Register inside its transaction.
func (s *DeviceService) register(ctx context.Context, g security.Grant, actor security.Subject, in registration, hash string) (*Device, error) {
	now := s.now().UTC()
	existing, err := Devices(s.db).Where("token_hash", "=", hash).First(ctx, g)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if _, err := security.Authorize(ctx, s.policy, actor, DeviceRegister, *existing); err != nil {
			return nil, err
		}
		previous := existing.UserID
		existing.Reassign(actor.ID, in.platform, in.app, in.locale, now)
		if _, err := existing.Save(ctx, g); err != nil {
			return nil, err
		}
		facts := deviceFacts(existing)
		facts["created"] = false
		facts["replaced_user_id"] = ""
		if previous != actor.ID {
			facts["replaced_user_id"] = previous
		}
		if err := s.audit.record(ctx, g, EventDeviceRegistered, existing.ID, facts); err != nil {
			return nil, err
		}
		return existing, nil
	}

	if err := s.makeRoom(ctx, g, actor.ID); err != nil {
		return nil, err
	}
	record, err := Devices(s.db).New()
	if err != nil {
		return nil, err
	}
	if record.ID, err = data.NewID(); err != nil {
		return nil, err
	}
	record.TenantID = data.Tenant(g)
	record.UserID = actor.ID
	record.TokenHash = hash
	if record.Token, err = s.sealer.seal(record.TenantID, record.ID, in.token); err != nil {
		return nil, err
	}
	record.Reassign(actor.ID, in.platform, in.app, in.locale, now)
	record.CreatedAt = now
	if _, err := record.Save(ctx, g); err != nil {
		return nil, err
	}
	facts := deviceFacts(record)
	facts["created"] = true
	if err := s.audit.record(ctx, g, EventDeviceRegistered, record.ID, facts); err != nil {
		return nil, err
	}
	return record, nil
}

// makeRoom removes the phones a user has beyond the limit, least recently seen
// first, to leave room for one more. The Grant is the registration's: the phones
// removed are the registering subject's own.
func (s *DeviceService) makeRoom(ctx context.Context, g security.Grant, userID string) error {
	held, err := Devices(s.db).Where("user_id", "=", userID).Count(ctx, g)
	if err != nil {
		return err
	}
	excess := int(held) - s.maxEach + 1
	if excess <= 0 {
		return nil
	}
	oldest, err := Devices(s.db).Where("user_id", "=", userID).
		OrderBy("last_seen_at").OrderBy("id").Limit(excess).Get(ctx, g)
	if err != nil {
		return err
	}
	for _, record := range oldest {
		if err := s.removeRecord(ctx, g, record, EventDeviceRemoved, RemovedLimit); err != nil {
			return err
		}
	}
	return nil
}

// holds reports whether the tenant holds a device with this digest.
func (s *DeviceService) holds(ctx context.Context, g security.Grant, hash string) bool {
	found, err := Devices(s.db).Where("token_hash", "=", hash).Exists(ctx, g)
	return err == nil && found
}

// Unregister removes the phone that holds this token from the subject's
// devices. It is idempotent: a token the subject does not hold -- unknown,
// already removed by Firebase, or somebody else's -- is already not theirs, and
// the end state is the one asked for.
func (s *DeviceService) Unregister(ctx context.Context, actor security.Subject, token string) error {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenBytes {
		errs := validation.Errors{}
		errs.Add("token", "The token is required.")
		return errs
	}
	g, err := security.Authorize(ctx, s.policy, actor, DeviceUnregister, Device{UserID: actor.ID})
	if err != nil {
		return err
	}
	record, err := Devices(s.db).Where("token_hash", "=", s.sealer.hash(data.Tenant(g), token)).First(ctx, g)
	if err != nil {
		return err
	}
	if record == nil {
		return nil
	}
	if _, err := security.Authorize(ctx, s.policy, actor, DeviceUnregister, *record); err != nil {
		if errors.Is(err, security.ErrForbidden) {
			return nil
		}
		return err
	}
	return s.removeRecord(ctx, g, record, EventDeviceUnregistered, "")
}

// UnregisterID removes one device by its identifier: the subject's own, or any
// of the tenant's for whoever holds DeviceManage. A device that is missing, in
// another tenant or somebody else's is ErrNotFound.
func (s *DeviceService) UnregisterID(ctx context.Context, actor security.Subject, id string) error {
	g, err := security.Authorize(ctx, s.policy, actor, DeviceUnregister, Device{UserID: actor.ID})
	if err != nil {
		return err
	}
	record, err := Devices(s.db).WhereKey(id).First(ctx, g)
	if err != nil {
		return err
	}
	if record == nil {
		return ErrNotFound
	}
	if _, err := security.Authorize(ctx, s.policy, actor, DeviceUnregister, *record); err != nil {
		return ErrNotFound
	}
	return s.removeRecord(ctx, g, record, EventDeviceUnregistered, "")
}

// ListForUser returns the devices of one user of the tenant, oldest first. An
// empty user is the subject themselves. Another user's takes DeviceManage.
func (s *DeviceService) ListForUser(ctx context.Context, actor security.Subject, userID string) ([]*Device, error) {
	if userID == "" {
		userID = actor.ID
	}
	g, err := security.Authorize(ctx, s.policy, actor, DeviceList, Device{UserID: userID})
	if err != nil {
		return nil, err
	}
	return Devices(s.db).Where("user_id", "=", userID).
		OrderBy("created_at").OrderBy("id").Limit(maxListed).Get(ctx, g)
}

// removeRecord deletes a device and records why, in one transaction.
func (s *DeviceService) removeRecord(ctx context.Context, g security.Grant, record *Device, event, reason string) error {
	return data.Transaction(ctx, s.db, func(ctx context.Context) error {
		if _, err := record.Delete(ctx, g); err != nil {
			return err
		}
		facts := deviceFacts(record)
		if reason != "" {
			facts["reason"] = reason
		}
		return s.audit.record(ctx, g, event, record.ID, facts)
	})
}

// The methods below serve the delivery of a notification and the scheduled
// clean. They are not use cases of a person: the Grant they are handed was
// issued for sending a notification, or for the clean, and each checks that
// before its first query.

// devicesFor returns the phones of a user to push to, most recently seen first.
// The Grant has to be the one the notification was authorized with.
func (s *DeviceService) devicesFor(ctx context.Context, g security.Grant, userID string) ([]*Device, error) {
	if err := g.Check(notifications.ActionSend); err != nil {
		return nil, err
	}
	if !security.ValidTenant(data.Tenant(g)) {
		return nil, fmt.Errorf("%w: the grant carries no usable tenant", security.ErrForbidden)
	}
	return Devices(s.db).Where("user_id", "=", userID).
		OrderByDesc("last_seen_at").OrderBy("id").Limit(maxNotified).Get(ctx, g)
}

// deviceByID returns one phone of the Grant's tenant for a delivery, or nil
// when it is gone.
func (s *DeviceService) deviceByID(ctx context.Context, g security.Grant, id string) (*Device, error) {
	if err := g.Check(notifications.ActionSend); err != nil {
		return nil, err
	}
	return Devices(s.db).WhereKey(id).First(ctx, g)
}

// removeDead deletes a phone Firebase said cannot receive any more.
func (s *DeviceService) removeDead(ctx context.Context, g security.Grant, record *Device, reason string) error {
	if err := g.Check(notifications.ActionSend); err != nil {
		return err
	}
	return s.removeRecord(ctx, g, record, EventDeviceRemoved, reason)
}

// clean removes the phones of the Grant's tenant that have not registered
// themselves since the cutoff.
func (s *DeviceService) clean(ctx context.Context, g security.Grant, cutoff time.Time) (int64, error) {
	if err := g.Check(DeviceClean); err != nil {
		return 0, err
	}
	var removed int64
	for {
		stale, err := Devices(s.db).Where("last_seen_at", "<", cutoff).OrderBy("id").Limit(cleanBatch).Get(ctx, g)
		if err != nil {
			return removed, err
		}
		for _, record := range stale {
			if err := s.removeRecord(ctx, g, record, EventDeviceRemoved, RemovedStale); err != nil {
				return removed, err
			}
			removed++
		}
		if len(stale) < cleanBatch {
			return removed, nil
		}
	}
}

// open reads the clear token of a device. It is reached by the delivery only.
func (s *DeviceService) open(record *Device) (string, error) {
	return s.sealer.open(record.TenantID, record.ID, record.Token)
}
