package unit_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/arandu-io/framework/data"
	"github.com/arandu-io/framework/security"
	"github.com/arandu-io/hesape/database/model"
	"github.com/arandu-io/hesape/database/query"

	fcm "github.com/hyz-is/arandu-fcm"
)

// The four properties this package exists to keep are checked here, and they
// are checked against the code rather than described in a document:
//
//  1. the policy denies every action unless the subject was given it;
//  2. the service authorizes before constructing or running the generated query;
//  3. the tenant comes from the Grant;
//  4. nothing reaches the database without passing through the first two.
//
// The fourth is checked by the handle these tests pass in. It wraps a nil
// *sql.DB, so any statement that were issued would panic and fail the test
// loudly -- which makes "the refusal happened before the Model" a fact the
// suite proves rather than a comment. The structural twin in audit_test.go keeps
// that order visible on every function that reaches the Model.

// everyAction is the whole set the policy answers about. A test that listed
// four of five would pass while the fifth was open.
var everyAction = []security.Action{
	fcm.DeviceRegister,
	fcm.DeviceUnregister,
	fcm.DeviceList,
	fcm.DeviceManage,
	fcm.DeviceClean,
}

const key = "0123456789abcdef0123456789abcdef"

// administrator is the most privileged subject by role an application can
// produce, and it carries no action. It is the one to test the default with: a
// policy that refuses an administrator by role refuses everyone.
func administrator() security.Subject {
	return security.Subject{ID: "user-1", Tenant: "acme", Roles: []string{"admin"}, Verified: true}
}

func holder(id string, actions ...security.Action) security.Subject {
	return security.Subject{ID: id, Tenant: "acme", Actions: actions, Verified: true}
}

func TestThePolicyDeniesEveryActionByDefault(t *testing.T) {
	t.Parallel()

	for _, action := range everyAction {
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()
			_, err := security.Authorize(context.Background(), fcm.DevicePolicy{},
				administrator(), action, fcm.Device{})
			if !errors.Is(err, security.ErrForbidden) {
				t.Fatalf("an unopened policy allowed %s: got %v, want ErrForbidden", action, err)
			}
		})
	}
}

func TestThePolicyOpensOnlyWhatTheSubjectWasGiven(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	own := fcm.Device{UserID: "ada"}
	ada := holder("ada", fcm.DeviceRegister, fcm.DeviceUnregister, fcm.DeviceList)

	for _, action := range []security.Action{fcm.DeviceRegister, fcm.DeviceUnregister, fcm.DeviceList} {
		if _, err := security.Authorize(ctx, fcm.DevicePolicy{}, ada, action, own); err != nil {
			t.Errorf("a member holding %s was refused on their own phone: %v", action, err)
		}
		if _, err := security.Authorize(ctx, fcm.DevicePolicy{}, holder("ada"), action, own); !errors.Is(err, security.ErrForbidden) {
			t.Errorf("a member without %s was allowed it: %v", action, err)
		}
	}

	// Somebody else's phone is theirs alone, unless the subject manages the
	// tenant.
	theirs := fcm.Device{ID: "d-1", TenantID: "acme", UserID: "bob"}
	for _, action := range []security.Action{fcm.DeviceUnregister, fcm.DeviceList} {
		if _, err := security.Authorize(ctx, fcm.DevicePolicy{}, ada, action, theirs); !errors.Is(err, security.ErrForbidden) {
			t.Errorf("ada was allowed %s on bob's phone: %v", action, err)
		}
		if _, err := security.Authorize(ctx, fcm.DevicePolicy{}, holder("root", fcm.DeviceManage), action, theirs); err != nil {
			t.Errorf("a manager was refused %s on bob's phone: %v", action, err)
		}
	}

	// Registering a token that was another user's takes it over: the proof is
	// the token, and the device that results is the registrant's.
	if _, err := security.Authorize(ctx, fcm.DevicePolicy{}, ada, fcm.DeviceRegister, theirs); err != nil {
		t.Errorf("registering a token another user held was refused: %v", err)
	}
}

func TestNobodyHoldsTheClean(t *testing.T) {
	t.Parallel()

	// Even a subject carrying every action there is, the clean included, is
	// refused: the scheduled task issues its own Grant.
	everything := holder("root", append(fcm.Actions(), fcm.DeviceClean)...)
	_, err := security.Authorize(context.Background(), fcm.DevicePolicy{}, everything, fcm.DeviceClean, fcm.Device{})
	if !errors.Is(err, security.ErrForbidden) {
		t.Fatalf("a subject was allowed the clean: %v", err)
	}
	if slices.Contains(fcm.Actions(), fcm.DeviceClean) {
		t.Fatal("Actions offers the clean to people")
	}
}

func TestOnlyListingIsAReadAction(t *testing.T) {
	t.Parallel()

	for _, action := range everyAction {
		want := action != fcm.DeviceList
		if got := (fcm.DevicePolicy{}).Writes(action); got != want {
			t.Errorf("Writes(%s) = %v, want %v", action, got, want)
		}
	}
}

func TestThePolicyDeniesADeviceOfAnotherTenant(t *testing.T) {
	t.Parallel()

	other := fcm.Device{ID: "d-1", TenantID: "globex", UserID: "ada"}
	subject := holder("ada", fcm.Actions()...)

	for _, action := range everyAction {
		err := fcm.DevicePolicy{}.Can(context.Background(), subject, action, other)
		if err == nil {
			t.Fatalf("the policy allowed %s on a device belonging to another tenant", action)
		}
		// The message is asserted because the tenant check is the one refusal
		// that has to survive somebody opening the actions below it.
		if !strings.Contains(err.Error(), "another tenant") {
			t.Fatalf("the refusal of %s did not name the tenant: %v", action, err)
		}
	}
}

func TestThePolicyDeniesAGuest(t *testing.T) {
	t.Parallel()

	for _, action := range everyAction {
		_, err := security.Authorize(context.Background(), fcm.DevicePolicy{},
			security.Guest("acme"), action, fcm.Device{})
		if !errors.Is(err, security.ErrForbidden) {
			t.Fatalf("a guest was allowed %s: got %v, want ErrForbidden", action, err)
		}
	}
}

func TestAuthorizeRefusesASubjectThatIsNobody(t *testing.T) {
	t.Parallel()

	// The zero Subject is a session that failed to load, not an anonymous
	// reader, and it is refused before the policy is consulted.
	_, err := security.Authorize(context.Background(), fcm.DevicePolicy{},
		security.Subject{}, fcm.DeviceRegister, fcm.Device{})
	if !errors.Is(err, security.ErrForbidden) {
		t.Fatalf("an empty subject was authorized: got %v, want ErrForbidden", err)
	}
}

// nilHandle is a handle over no database.
//
// Any statement issued through it panics, which is what makes these tests prove
// that the refusal came first: a service that reached the Model before
// authorizing would crash here rather than pass.
func nilHandle() *data.DB { return data.Wrap(nil, data.DialectSQLite) }

func TestTheServiceRefusesBeforeReachingTheModel(t *testing.T) {
	t.Parallel()

	// A nil handle makes even construction of Devices panic, at the
	// GetQueryGrammar the query asks the handle for. This catches moving the
	// generated query constructor -- not only its terminal -- ahead of
	// authorization.
	service, err := fcm.NewDeviceService(nil, fcm.Config{Key: []byte(key)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	nobody := administrator() // by role only: holds no action

	if _, err := service.Register(ctx, nobody, fcm.RegisterRequest{Token: "tok-12345678", Platform: "android"}); !errors.Is(err, security.ErrForbidden) {
		t.Fatalf("Register reached the Model before the policy refusal: %v", err)
	}
	if err := service.Unregister(ctx, nobody, "tok-12345678"); !errors.Is(err, security.ErrForbidden) {
		t.Fatalf("Unregister reached the Model before the policy refusal: %v", err)
	}
	if err := service.UnregisterID(ctx, nobody, "d-1"); !errors.Is(err, security.ErrForbidden) {
		t.Fatalf("UnregisterID reached the Model before the policy refusal: %v", err)
	}
	if _, err := service.ListForUser(ctx, nobody, ""); !errors.Is(err, security.ErrForbidden) {
		t.Fatalf("ListForUser reached the Model before the policy refusal: %v", err)
	}
}

func TestTheRequestIsValidatedBeforeAnythingIsAuthorized(t *testing.T) {
	t.Parallel()

	service, _ := fcm.NewDeviceService(nil, fcm.Config{Key: []byte(key)})
	_, err := service.Register(context.Background(), administrator(), fcm.RegisterRequest{})
	var errs interface{ Any() bool }
	if !errors.As(err, &errs) || !errs.Any() {
		t.Fatalf("an empty request answered %v, want validation errors", err)
	}
}

func TestNewDeviceServiceRefusesWhatCannotSeal(t *testing.T) {
	t.Parallel()

	if _, err := fcm.NewDeviceService(nil, fcm.Config{Key: []byte("short")}); err == nil {
		t.Error("a short key was accepted")
	}
	if _, err := fcm.NewDeviceService(nil, fcm.Config{Key: []byte(key), MaxDevicesPerUser: -1}); err == nil {
		t.Error("a negative device limit was accepted")
	}
}

// recordingHandle runs nothing and keeps every statement it is handed, so a test
// reads what the table compiles instead of reading its settings.
type recordingHandle struct {
	*data.DB
	statements []recordedStatement
}

type recordedStatement struct {
	sql      string
	bindings []any
}

// newRecordingHandle borrows the grammar and the processor of an SQLite handle
// over no database, and answers every statement itself.
func newRecordingHandle() *recordingHandle { return &recordingHandle{DB: nilHandle()} }

func (h *recordingHandle) record(sql string, bindings []any) {
	h.statements = append(h.statements, recordedStatement{sql: sql, bindings: slices.Clone(bindings)})
}

func (h *recordingHandle) Select(_ context.Context, sql string, bindings []any, _ bool) ([]query.Record, error) {
	h.record(sql, bindings)
	return nil, nil
}

func (h *recordingHandle) Insert(_ context.Context, sql string, bindings []any) (bool, error) {
	h.record(sql, bindings)
	return true, nil
}

func (h *recordingHandle) Update(_ context.Context, sql string, bindings []any) (int64, error) {
	h.record(sql, bindings)
	return 1, nil
}

func (h *recordingHandle) Delete(_ context.Context, sql string, bindings []any) (int64, error) {
	h.record(sql, bindings)
	return 1, nil
}

func (h *recordingHandle) Statement(_ context.Context, sql string, bindings []any) (bool, error) {
	h.record(sql, bindings)
	return true, nil
}

// first is the first recorded statement that starts with verb.
func (h *recordingHandle) first(t *testing.T, verb string) recordedStatement {
	t.Helper()
	for _, statement := range h.statements {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(statement.sql)), verb) {
			return statement
		}
	}
	t.Fatalf("no %s statement was issued; recorded %v", verb, h.statements)
	return recordedStatement{}
}

// TestDevicesReturnsAWiredTenantScopedQuery holds what deviceTable declares,
// through what it makes the generated query do: the table it reads, a key the
// application writes and the engine never fills, and a tenant_id taken from the
// Grant on the way in and on the way out.
func TestDevicesReturnsAWiredTenantScopedQuery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	handle := newRecordingHandle()
	table := fcm.Devices(handle).Base().Table()
	if table.Name() != "fcm_devices" {
		t.Fatalf("Devices table = %q, want fcm_devices", table.Name())
	}
	if key := table.MorphModel(handle); key.GetKeyName() != "id" || key.GetKeyType() != "string" {
		t.Fatalf("Devices key is %q of type %q; want id, as text", key.GetKeyName(), key.GetKeyType())
	}

	row, err := fcm.Devices(handle).New()
	if err != nil {
		t.Fatalf("building a row: %v", err)
	}
	if row.Table() != table {
		t.Fatal("Devices returned an entity whose embedded Model is not wired to its table")
	}

	row.ID = "device-1"
	//arandu:system-grant the test plays the application that authorized the write.
	if _, err := row.Save(ctx, security.SystemGrant(fcm.DeviceRegister, "acme")); err != nil {
		t.Fatalf("saving through the recording handle: %v", err)
	}
	insert := handle.first(t, "insert")
	if !slices.Contains(insert.bindings, any("device-1")) || row.ID != "device-1" {
		t.Fatalf("the insert %q %v does not write the application key, or the row lost it (%q)", insert.sql, insert.bindings, row.ID)
	}
	if !strings.Contains(insert.sql, "tenant_id") || !slices.Contains(insert.bindings, any("acme")) {
		t.Fatalf("the insert %q %v does not stamp tenant_id with the Grant's tenant", insert.sql, insert.bindings)
	}

	//arandu:system-grant the test plays the application that authorized the read.
	if _, err := fcm.Devices(handle).WhereKey("device-1").First(ctx, security.SystemGrant(fcm.DeviceList, "acme")); err != nil {
		t.Fatalf("reading through the recording handle: %v", err)
	}
	read := handle.first(t, "select")
	if !strings.Contains(read.sql, "tenant_id") || !slices.Contains(read.bindings, any("acme")) {
		t.Fatalf("the read %q %v is not filtered by tenant_id with the Grant's tenant", read.sql, read.bindings)
	}
}

// TestTheMissingDeviceIsTheModelsNotFound holds the one link between this
// package's sentinel and the router's answer. The router answers
// model.ErrModelNotFound with 404 and has never heard of ErrNotFound, so the
// sentinel is a 404 only while it wraps the model's.
func TestTheMissingDeviceIsTheModelsNotFound(t *testing.T) {
	t.Parallel()

	if !errors.Is(fcm.ErrNotFound, model.ErrModelNotFound) {
		t.Fatal("ErrNotFound does not wrap model.ErrModelNotFound, so the router would answer a missing device with 500")
	}
}

func TestASystemGrantWithoutATenantReachesNothing(t *testing.T) {
	t.Parallel()

	// A system grant with no tenant names no customer. The Model refuses it
	// while preparing the query, before the nil handle can issue a statement.
	//arandu:system-grant the test proves a grant with no tenant reaches nothing.
	_, err := fcm.Devices(nilHandle()).WhereKey("device-1").First(
		context.Background(), security.SystemGrant(fcm.DeviceList, ""))
	if !errors.Is(err, model.ErrNoTenant) {
		t.Fatalf("a system grant with no tenant returned %v, want ErrNoTenant", err)
	}
}

func TestTheTenantComesFromTheGrant(t *testing.T) {
	t.Parallel()

	//arandu:system-grant the test reads the tenant a grant carries.
	g := security.SystemGrant(fcm.DeviceList, "acme")
	if got := data.Tenant(g); got != "acme" {
		t.Fatalf("data.Tenant(g) = %q, want %q", got, "acme")
	}
	if got := data.Tenant(security.Grant{}); got != "" {
		t.Fatalf("the zero Grant carries the tenant %q, want none", got)
	}
}

func TestTheDeviceNeverPrintsOrEncodesItsToken(t *testing.T) {
	t.Parallel()

	d := &fcm.Device{ID: "d-1", TenantID: "acme", UserID: "ada", Token: "SEALED-TOKEN-VALUE", TokenHash: "HASH-VALUE", Platform: fcm.PlatformIOS}
	raw, err := d.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"json": string(raw), "string": d.String(), "log": d.LogValue().String()} {
		for _, secret := range []string{"SEALED-TOKEN-VALUE", "HASH-VALUE"} {
			if strings.Contains(text, secret) {
				t.Errorf("the %s form of a device carries %s: %s", name, secret, text)
			}
		}
	}
}

func TestTheRequestValidatesItsInput(t *testing.T) {
	t.Parallel()

	if errs := (fcm.RegisterRequest{}).Validate(); !errs.Any() {
		t.Fatal("an empty request validated")
	}
	if errs := (fcm.RegisterRequest{Token: "tok-12345678", Platform: "iOS", App: "com.example.app", Locale: "pt_BR"}).Validate(); errs.Any() {
		t.Fatalf("a valid request was rejected: %v", errs)
	}
	if p, ok := fcm.ParsePlatform(" Android "); !ok || p != fcm.PlatformAndroid {
		t.Fatalf("ParsePlatform = %q, %v", p, ok)
	}
}

func TestTheConfigurationRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()

	good := fcm.Config{Sender: &fcm.Fake{}, Key: []byte(key)}
	if err := good.Validate(); err != nil {
		t.Fatalf("a valid configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*fcm.Config){
		"no credentials or sender": func(c *fcm.Config) { c.Sender = nil },
		"short key":                func(c *fcm.Config) { c.Key = []byte("short") },
		"relative prefix":          func(c *fcm.Config) { c.Prefix = "fcm" },
		"trailing slash":           func(c *fcm.Config) { c.Prefix = "/fcm/" },
		"framework namespace":      func(c *fcm.Config) { c.Prefix = "/_arandu/fcm" },
		"negative tries":           func(c *fcm.Config) { c.Tries = -1 },
		"too many phones":          func(c *fcm.Config) { c.MaxDevicesPerUser = fcm.MaxDevicesPerUserLimit + 1 },
		"plain http base":          func(c *fcm.Config) { c.APIBase = "http://fcm.example.com" },
	} {
		cfg := good
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("the configuration with %s was accepted", name)
		}
	}
}
