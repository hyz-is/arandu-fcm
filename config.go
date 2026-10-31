package fcm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/arandu-io/hesape/queue"
)

// The defaults for the optional settings. They are constants rather than
// literals inside Config.withDefaults, so the value a reader finds here is the
// value the package uses.
const (
	// DefaultPrefix is where the optional routes are mounted when Config leaves
	// Prefix empty.
	DefaultPrefix = "/fcm/devices"
	// MinKeyBytes is the shortest application key the module seals tokens with.
	MinKeyBytes = 32
	// DefaultMaxDevicesPerUser is how many phones the module keeps for one
	// person when Config leaves MaxDevicesPerUser at zero.
	DefaultMaxDevicesPerUser = 10
	// MaxDevicesPerUserLimit is the ceiling MaxDevicesPerUser is refused above.
	MaxDevicesPerUserLimit = 50
	// DefaultStaleAfterDays is how long a phone may go without registering
	// itself before the clean removes it. Firebase expires a token that has
	// been inactive for 270 days.
	DefaultStaleAfterDays = 270
	// DefaultCleanSpec is when the scheduled clean runs: every day at 03:40.
	DefaultCleanSpec = "40 3 * * *"
	// DefaultNotifiableType is the NotifiableType of the recipients whose
	// NotifiableID is a user that owns phones.
	DefaultNotifiableType = "user"
)

// reservedPrefix is the namespace the framework keeps for itself.
const reservedPrefix = "/_arandu"

// Config is what the application passes when it wires this package.
//
// A typed struct rather than a map: a misspelled key in a map is a setting that
// silently keeps its default, and here a field that does not exist does not
// compile.
//
// It names no tenant, and that is deliberate. The tenant of every statement is
// the one the Grant carries, from the session of whoever is asking or from the
// Grant the notification was authorized with; a tenant written here would be a
// second place for one to come from.
//
// It holds secrets -- the application key and the service account -- and prints,
// logs and encodes without them.
type Config struct {
	// Credentials is the Firebase service account. Read it from the environment
	// with CredentialsFromEnv. Required unless Sender is set.
	Credentials Credentials

	// Key is the application's key. The registration tokens are sealed, and
	// digested for lookup, with keys derived from it for this module alone.
	// Required, at least MinKeyBytes.
	Key []byte

	// Queue is where pushes are put to be delivered by a worker, with the retry
	// and backoff the queue gives. Nil sends each push before the notification
	// returns, with no retry. Register the handler with RegisterJobHandlers and
	// run the worker for QueueName.
	Queue queue.Queue

	// QueueName is the queue the pushes go on. Empty means DefaultQueue.
	QueueName string

	// Tries is how many deliveries a push job gets before it is parked. Zero
	// means DefaultTries.
	Tries int

	// Routes mounts the JSON routes an application's mobile client calls to
	// register and remove its token. They are off by default: an application
	// that wraps the DeviceService in routes of its own leaves them off.
	Routes bool

	// Prefix is the path the routes are mounted under. Empty means
	// DefaultPrefix.
	Prefix string

	// NotifiableType is the NotifiableType of the recipients whose NotifiableID
	// is the user that owns the phones. Empty means DefaultNotifiableType.
	NotifiableType string

	// MaxDevicesPerUser is how many phones are kept for one person; registering
	// another removes the one seen least recently. Zero means
	// DefaultMaxDevicesPerUser.
	MaxDevicesPerUser int

	// StaleAfterDays is how long a phone may go without registering itself
	// before the clean removes it. Zero means DefaultStaleAfterDays.
	StaleAfterDays int

	// CleanSpec is the five-field cron expression the clean runs on. Empty means
	// DefaultCleanSpec; "-" schedules none.
	CleanSpec string

	// APIBase is where Firebase answers. Empty means DefaultAPIBase. It is for
	// a local emulator or a test server, and it has to be https unless the host
	// is loopback.
	APIBase string

	// HTTPClient carries the calls to Firebase and to the token address. Nil
	// means a client with a timeout that follows no redirect.
	HTTPClient *http.Client

	// Sender replaces the client that reaches Firebase, for the application's
	// tests: with Fake, nothing leaves the process and Credentials is not
	// needed.
	Sender Sender

	// Logger is where the module logs what it did to a device and why a push
	// failed, never a token or a credential. Nil means slog.Default().
	Logger *slog.Logger

	// Clock is the time the module reads. Nil means time.Now.
	Clock func() time.Time
}

// Validate reports what the configuration cannot be used with.
//
// It is called by New, so an application with a setting that cannot work fails
// where it is wired rather than on the first request that needed it.
func (c Config) Validate() error {
	if c.Sender == nil && c.Credentials.IsZero() {
		return errors.New("fcm: Config.Credentials is empty: read the service account with CredentialsFromEnv, or pass a Sender for tests")
	}
	if len(c.Key) < MinKeyBytes {
		return fmt.Errorf("fcm: Config.Key has %d bytes and needs at least %d: pass the application's key", len(c.Key), MinKeyBytes)
	}
	if c.Prefix != "" {
		if c.Prefix[0] != '/' || c.Prefix == "/" || strings.HasSuffix(c.Prefix, "/") {
			return fmt.Errorf("fcm: Config.Prefix is %q and has to start with / and not end with one", c.Prefix)
		}
		if err := validateRoutePrefix(c.Prefix); err != nil {
			return err
		}
	}
	if c.Tries < 0 {
		return errors.New("fcm: Config.Tries cannot be negative")
	}
	if c.MaxDevicesPerUser < 0 || c.MaxDevicesPerUser > MaxDevicesPerUserLimit {
		return fmt.Errorf("fcm: Config.MaxDevicesPerUser is %d, and has to be between 0 and %d, where 0 means %d",
			c.MaxDevicesPerUser, MaxDevicesPerUserLimit, DefaultMaxDevicesPerUser)
	}
	if c.StaleAfterDays < 0 {
		return errors.New("fcm: Config.StaleAfterDays cannot be negative")
	}
	if c.QueueName != "" && strings.ContainsAny(c.QueueName, " \t\n") {
		return fmt.Errorf("fcm: Config.QueueName %q cannot hold spaces", c.QueueName)
	}
	if c.APIBase != "" {
		if err := checkEndpoint("Config.APIBase", c.APIBase); err != nil {
			return err
		}
	}
	return nil
}

// validateRoutePrefix asks the standard library to parse the exact patterns the
// module will register. Its parser is not exported and reports invalid patterns
// by panic, so the throwaway mux turns that boot-time panic into the
// configuration error New promises.
func validateRoutePrefix(prefix string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("fcm: Config.Prefix %q cannot be registered as a route path", prefix)
		}
	}()
	if strings.HasPrefix(prefix, reservedPrefix) {
		return fmt.Errorf("fcm: Config.Prefix %q is inside %s, which the framework keeps for itself", prefix, reservedPrefix)
	}
	handler := http.NotFoundHandler()
	mux := http.NewServeMux()
	mux.Handle(http.MethodGet+" "+prefix, handler)
	mux.Handle(http.MethodPost+" "+prefix, handler)
	mux.Handle(http.MethodPost+" "+prefix+"/unregister", handler)
	mux.Handle(http.MethodDelete+" "+prefix+"/{id}", handler)
	return nil
}

// withDefaults returns the configuration with the optional fields filled in.
//
// It runs after Validate and never before: filling a default in first would
// hide the value somebody actually wrote from the check that would have refused
// it.
func (c Config) withDefaults() Config {
	if c.Prefix == "" {
		c.Prefix = DefaultPrefix
	}
	if c.QueueName == "" {
		c.QueueName = DefaultQueue
	}
	if c.Tries == 0 {
		c.Tries = DefaultTries
	}
	if c.NotifiableType == "" {
		c.NotifiableType = DefaultNotifiableType
	}
	if c.MaxDevicesPerUser == 0 {
		c.MaxDevicesPerUser = DefaultMaxDevicesPerUser
	}
	if c.StaleAfterDays == 0 {
		c.StaleAfterDays = DefaultStaleAfterDays
	}
	if c.CleanSpec == "" {
		c.CleanSpec = DefaultCleanSpec
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

// summary is the configuration as a log line or an encoding may carry it: every
// setting that is not a secret, and a marker where a secret is.
func (c Config) summary() map[string]any {
	return map[string]any{
		"credentials":          c.Credentials.String(),
		"key":                  redacted,
		"routes":               c.Routes,
		"prefix":               c.Prefix,
		"queue":                c.Queue != nil,
		"queue_name":           c.QueueName,
		"tries":                c.Tries,
		"notifiable_type":      c.NotifiableType,
		"max_devices_per_user": c.MaxDevicesPerUser,
		"stale_after_days":     c.StaleAfterDays,
		"clean_spec":           c.CleanSpec,
		"api_base":             c.APIBase,
	}
}

// String describes the configuration without its secrets.
func (c Config) String() string { return fmt.Sprintf("fcm.Config%v", c.summary()) }

// GoString is what %#v prints.
func (c Config) GoString() string { return c.String() }

// Format answers every fmt verb with String, so no verb reaches the key or the
// service account.
func (c Config) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// LogValue is what slog records.
func (c Config) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, len(c.summary()))
	for k, v := range c.summary() {
		attrs = append(attrs, slog.Any(k, v))
	}
	return slog.GroupValue(attrs...)
}

// MarshalJSON encodes the configuration without its secrets.
func (c Config) MarshalJSON() ([]byte, error) { return json.Marshal(c.summary()) }
