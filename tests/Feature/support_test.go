package feature_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arandu-io/framework/data"
	fhttp "github.com/arandu-io/framework/http"
	"github.com/arandu-io/framework/security"
	hedatabase "github.com/arandu-io/hesape/database"
	pgxconnector "github.com/arandu-io/hesape/database/connectors/pgx"
	sqliteconnector "github.com/arandu-io/hesape/database/connectors/sqlite"
	"github.com/arandu-io/hesape/events"
	"github.com/arandu-io/hesape/notifications"
	"github.com/arandu-io/hesape/queue"

	fcm "github.com/hyz-is/arandu-fcm"
)

const (
	appKey = "0123456789abcdef0123456789abcdef"
	dsn    = "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(20000)"

	acme   = "acme"
	globex = "globex"

	// deviceToken is a registration token the tests send to. Its text is the
	// secret the leak tests look for.
	deviceToken = "tok-AAAA1111:APA91bF-registration-token-of-ada-android"
	iosToken    = "tok-BBBB2222:APA91bG-registration-token-of-ada-iphone"
)

// testKey is the service account's RSA key, generated once for the run.
var (
	keyOnce sync.Once
	rsaKey  *rsa.PrivateKey
	keyPEM  string
)

func serviceKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		rsaKey, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
		if err != nil {
			panic(err)
		}
		keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	})
	return rsaKey, keyPEM
}

// reply is what the fake answers a request with.
type reply struct {
	Status int
	Header map[string]string
	Body   string
}

// sentMessage is one request the fake Firebase received on the send endpoint.
type sentMessage struct {
	Header http.Header
	Body   []byte
}

// message decodes the body into the "message" object.
func (m sentMessage) message(t *testing.T) map[string]any {
	t.Helper()
	var envelope struct {
		Message map[string]any `json:"message"`
	}
	if err := json.Unmarshal(m.Body, &envelope); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, m.Body)
	}
	return envelope.Message
}

// firebase is a fake of the two services the module talks to: the OAuth2 token
// endpoint of Google and the FCM v1 send endpoint.
type firebase struct {
	t       *testing.T
	server  *httptest.Server
	project string
	email   string
	keyID   string

	mu         sync.Mutex
	tokenCalls []map[string]any
	badTokens  []string
	issued     int
	expiresIn  int64
	sends      []sentMessage
	tokens     []string

	// script answers the nth (1-based) send; nil or a nil return is success.
	script func(n int, m sentMessage) *reply
	// tokenScript answers the nth token request; nil or a nil return issues a
	// token.
	tokenScript func(n int) *reply
}

func newFirebase(t *testing.T) *firebase {
	t.Helper()
	key, _ := serviceKey(t)
	f := &firebase{t: t, project: "demo-project", email: "push@demo-project.iam.gserviceaccount.com", keyID: "kid-0001", expiresIn: 3600}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) { f.token(w, r, &key.PublicKey) })
	mux.HandleFunc("POST /v1/projects/{project}/messages:send", f.send)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// credentialsJSON is the service account file, pointing at the fake.
func (f *firebase) credentialsJSON(t *testing.T) []byte {
	_, pemKey := serviceKey(t)
	raw, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": f.project, "private_key_id": f.keyID,
		"private_key": pemKey, "client_email": f.email, "token_uri": f.server.URL + "/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *firebase) credentials(t *testing.T) fcm.Credentials {
	t.Helper()
	c, err := fcm.ParseCredentials(f.credentialsJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func write(w http.ResponseWriter, r *reply) {
	for k, v := range r.Header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(r.Status)
	_, _ = io.WriteString(w, r.Body)
}

func (f *firebase) token(w http.ResponseWriter, r *http.Request, pub *rsa.PublicKey) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		f.badTokens = append(f.badTokens, "grant_type "+r.Form.Get("grant_type"))
		write(w, &reply{Status: 400, Body: `{"error":"unsupported_grant_type"}`})
		return
	}
	assertion := r.Form.Get("assertion")
	claims, problem := f.verify(assertion, pub)
	if problem != "" {
		f.badTokens = append(f.badTokens, problem)
		// An error body that echoes everything it was given: the module has to
		// keep all of it out of its errors.
		body, _ := json.Marshal(map[string]string{
			"error":             "invalid_grant",
			"error_description": "bad assertion " + assertion + " for " + f.email,
		})
		write(w, &reply{Status: 400, Body: string(body)})
		return
	}
	f.tokenCalls = append(f.tokenCalls, claims)
	if f.tokenScript != nil {
		if rep := f.tokenScript(len(f.tokenCalls)); rep != nil {
			write(w, rep)
			return
		}
	}
	f.issued++
	access := fmt.Sprintf("ya29.access-token-number-%d-SECRET", f.issued)
	f.tokens = append(f.tokens, access)
	body, _ := json.Marshal(map[string]any{"access_token": access, "expires_in": f.expiresIn, "token_type": "Bearer"})
	write(w, &reply{Status: 200, Body: string(body)})
}

// verify checks the JWT like Google would and answers its claims.
func (f *firebase) verify(assertion string, pub *rsa.PublicKey) (map[string]any, string) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return nil, "assertion is not a JWT"
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, "signature is not base64url"
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], signature); err != nil {
		return nil, "signature does not verify"
	}
	var header map[string]string
	rawHeader, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(rawHeader, &header)
	if header["alg"] != "RS256" || header["typ"] != "JWT" || header["kid"] != f.keyID {
		return nil, fmt.Sprintf("header %v", header)
	}
	var claims map[string]any
	rawClaims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return nil, "claims are not JSON"
	}
	if claims["iss"] != f.email || claims["scope"] != fcm.Scope || claims["aud"] != f.server.URL+"/token" {
		return nil, fmt.Sprintf("claims %v", claims)
	}
	if iat, exp := claims["iat"].(float64), claims["exp"].(float64); exp-iat != 3600 {
		return nil, "lifetime is not an hour"
	}
	return claims, ""
}

func (f *firebase) send(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	sent := sentMessage{Header: r.Header.Clone(), Body: body}
	if r.PathValue("project") != f.project {
		write(w, &reply{Status: 404, Body: `{"error":{"code":404,"status":"NOT_FOUND","message":"project"}}`})
		return
	}
	f.sends = append(f.sends, sent)
	n := len(f.sends)
	if f.script != nil {
		if rep := f.script(n, sent); rep != nil {
			write(w, rep)
			return
		}
	}
	write(w, &reply{Status: 200, Body: fmt.Sprintf(`{"name":"projects/%s/messages/0:%d"}`, f.project, n)})
}

func (f *firebase) sent() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sends...)
}

func (f *firebase) tokenRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokenCalls)
}

func (f *firebase) problems() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.badTokens...)
}

// The error bodies of the v1 API.
const (
	unregisteredBody = `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`
	badTokenBody     = `{"error":{"code":400,"message":"The registration token is not a valid FCM registration token","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"message.token","description":"The registration token is not a valid FCM registration token"}]},{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"INVALID_ARGUMENT"}]}}`
	badPayloadBody   = `{"error":{"code":400,"message":"Invalid value at message.data","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"message.data","description":"bad"}]}]}}`
	quotaBody        = `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"QUOTA_EXCEEDED"}]}}`
	unavailableBody  = `{"error":{"code":503,"message":"The server is overloaded","status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNAVAILABLE"}]}}`
)

// clock is a time the test moves.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Now().UTC().Truncate(time.Second)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// logBuffer is a log destination that can be read while the module writes.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// world is an application with the module over a real database, the jobs table,
// the events outbox, a worker, a notifier and a fake Firebase.
type world struct {
	t        *testing.T
	db       *data.DB
	module   *fcm.Module
	fb       *firebase
	clock    *clock
	logs     *logBuffer
	jobs     *queue.DatabaseQueue
	worker   *queue.Worker
	notifier *notifications.Notifier
	sessions *security.SessionStore
	router   *fhttp.Router
	cfg      fcm.Config
	// resolve turns the account in a session into the subject a request acts
	// as; a test replaces it to give an account other actions.
	resolve func(security.Subject) security.Subject
}

// postgresEnv names the variable that points the suite at a PostgreSQL server
// instead of the SQLite file it runs on by default. Each test gets a schema of
// its own on that server, dropped when it ends.
const postgresEnv = "FCM_TEST_DATABASE_URL"

// openDatabase opens the database a test runs on: a SQLite file in the test's
// directory, or a schema of the PostgreSQL server named by FCM_TEST_DATABASE_URL.
func openDatabase(t *testing.T) (*data.DB, *hedatabase.Connection) {
	t.Helper()
	if url := os.Getenv(postgresEnv); url != "" {
		return openPostgres(t, url)
	}
	path := filepath.Join(t.TempDir(), "fcm.sqlite")
	handle, err := sql.Open(sqliteconnector.SQLiteConnector{}.DriverName(), "file:"+path+dsn)
	if err != nil {
		t.Fatal(err)
	}
	handle.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = handle.Close() })
	connection := hedatabase.NewConnection(handle, path, "", map[string]any{"driver": string(hedatabase.DialectSQLite), "name": "fcm-test"})
	return data.Wrap(handle, data.DialectSQLite), connection
}

func openPostgres(t *testing.T, url string) (*data.DB, *hedatabase.Connection) {
	t.Helper()
	admin, err := sql.Open(pgxconnector.PostgresConnector{}.DriverName(), url)
	if err != nil {
		t.Fatal(err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schemaName := "fcm_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec("CREATE SCHEMA " + schemaName); err != nil {
		t.Fatalf("creating the schema on %s: %v", postgresEnv, err)
	}
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	handle, err := sql.Open(pgxconnector.PostgresConnector{}.DriverName(), url+separator+"search_path="+schemaName)
	if err != nil {
		t.Fatal(err)
	}
	handle.SetMaxOpenConns(4)
	t.Cleanup(func() {
		_ = handle.Close()
		_, _ = admin.Exec("DROP SCHEMA " + schemaName + " CASCADE")
		_ = admin.Close()
	})
	connection := hedatabase.NewConnection(handle, schemaName, "", map[string]any{"driver": string(hedatabase.DialectPostgres), "name": "fcm-test"})
	return data.Wrap(handle, data.DialectPostgres), connection
}

// install builds the world. configure adjusts the configuration after the
// defaults of the test are set.
func install(t *testing.T, queued bool, configure func(*fcm.Config)) *world {
	t.Helper()
	db, connection := openDatabase(t)

	w := &world{t: t, db: db, fb: newFirebase(t), clock: newClock(), logs: &logBuffer{}}
	w.jobs = queue.NewDatabaseQueue(db)
	w.sessions = security.NewSessionStore([]byte(appKey), time.Hour, false, security.NewMemoryBackend())
	w.resolve = func(who security.Subject) security.Subject { return subjectFor(who.Tenant, who.ID) }
	w.sessions.ResolveSubjects(func(_ context.Context, who security.Subject) (security.Subject, error) {
		return w.resolve(who), nil
	})

	w.cfg = fcm.Config{
		Credentials: w.fb.credentials(t),
		Key:         []byte(appKey),
		APIBase:     w.fb.server.URL,
		Clock:       w.clock.now,
		Logger:      slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if configure != nil {
		configure(&w.cfg)
	}
	if queued {
		w.cfg.Queue = w.jobs
	}
	module, err := fcm.New(w.cfg, db, w.sessions)
	if err != nil {
		t.Fatal(err)
	}
	w.module = module

	schema := hedatabase.ForMigrations(connection)
	for _, m := range append(append(w.jobs.Migrations(), events.NewModule().Migrations()...), module.Migrations()...) {
		if err := m.Up(context.Background(), schema); err != nil {
			t.Fatalf("applying %s: %v", m.GetName(), err)
		}
	}
	w.worker = queue.NewWorker(w.jobs, queue.WorkerOptions{Queue: fcm.DefaultQueue, Sleep: time.Millisecond})
	if err := module.RegisterJobHandlers(w.worker); err != nil {
		t.Fatal(err)
	}
	w.notifier = notifications.New([]notifications.Channel{module.Channel()})
	w.router = fhttp.NewRouter()
	module.Routes(w.router.ForModule(module.Name()))
	return w
}

// subjectFor is a member of the tenant who may manage their own phones.
func subjectFor(tenant, id string) security.Subject {
	actions := []security.Action{fcm.DeviceRegister, fcm.DeviceUnregister, fcm.DeviceList}
	if strings.HasPrefix(id, "admin") {
		actions = fcm.Actions()
	}
	return security.Subject{ID: id, Tenant: tenant, Actions: actions, Verified: true}
}

func member(tenant, id string) security.Subject { return subjectFor(tenant, id) }

// register registers a phone for the subject and fails the test on an error.
func (w *world) register(who security.Subject, token string, platform string) *fcm.Device {
	w.t.Helper()
	d, err := w.module.Devices().Register(context.Background(), who, fcm.RegisterRequest{
		Token: token, Platform: platform, App: "com.example.app", Locale: "pt-BR",
	})
	if err != nil {
		w.t.Fatalf("registering %s: %v", platform, err)
	}
	return d
}

// devices lists the phones of a user of a tenant, as the tenant's administrator.
func (w *world) devices(tenant, user string) []*fcm.Device {
	w.t.Helper()
	list, err := w.module.Devices().ListForUser(context.Background(), member(tenant, "admin-"+tenant), user)
	if err != nil {
		w.t.Fatal(err)
	}
	return list
}

// count is how many devices the table holds for a tenant, bypassing the module.
func (w *world) count(tenant string) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM fcm_devices WHERE tenant_id = ?", tenant).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// user is a recipient of notifications.
type user struct{ id string }

func (u user) NotifiableID() string                      { return u.id }
func (u user) NotifiableType() string                    { return "user" }
func (u user) RouteFor(notifications.ChannelName) string { return "" }
func (u user) PreferredLocale() string                   { return "" }

// ping is a notification that goes by push.
type ping struct{ push fcm.Push }

func (ping) Key() notifications.Key { return "order.shipped" }
func (ping) Via(notifications.Notifiable) []notifications.ChannelName {
	return []notifications.ChannelName{fcm.ChannelFCM}
}
func (p ping) ToPush(notifications.Notifiable) fcm.Push { return p.push }

// notify sends a notification to a user of a tenant through the Notifier.
func (w *world) notify(tenant, userID string, n notifications.Notification) error {
	w.t.Helper()
	//arandu:system-grant the test plays the application that authorized the send.
	g := security.SystemGrant(notifications.ActionSend, tenant)
	return w.notifier.Send(context.Background(), g, user{id: userID}, n)
}

// runJob runs the next job on the push queue and answers the handler's error.
func (w *world) runJob() error {
	w.t.Helper()
	return w.worker.RunNextJob(context.Background())
}

// size is how many jobs the push queue holds, waiting or in flight.
func (w *world) size() int {
	w.t.Helper()
	n, err := w.jobs.Size(context.Background(), fcm.DefaultQueue)
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}

// pending is how many jobs wait to be taken now.
func (w *world) pending() int {
	w.t.Helper()
	n, err := w.jobs.PendingSize(context.Background(), fcm.DefaultQueue)
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}

// failedJobs is how many jobs were parked.
func (w *world) failedJobs() int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM jobs WHERE failed_at IS NOT NULL").Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// events are the names stored in the outbox, oldest first.
func (w *world) events() []string {
	w.t.Helper()
	rows, err := w.db.QueryContext(context.Background(), "SELECT event FROM outbox ORDER BY occurred_at, id")
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			w.t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}
