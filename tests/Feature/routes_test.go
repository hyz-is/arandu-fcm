package feature_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/arandu-io/framework/data"
	fhttp "github.com/arandu-io/framework/http"
	"github.com/arandu-io/framework/http/middleware"
	"github.com/arandu-io/framework/security"

	fcm "github.com/hyz-is/arandu-fcm"
)

// reservedPrefix is the namespace the framework keeps for itself: the health
// probe, the reload endpoint, the development console, the addressed assets.
// A module that registers under it is refused when the application boots, by
// name -- in the process of whoever installed this package, after it was
// published. Here the same rule is a failing test, in the repository that can
// still fix it.
const reservedPrefix = "/_arandu"

func routed(t *testing.T) *world {
	t.Helper()
	return install(t, false, func(c *fcm.Config) { c.Routes = true })
}

// signIn starts a session for the subject and returns the cookies a client
// would send back.
func (w *world) signIn(who security.Subject) []*http.Cookie {
	w.t.Helper()
	rec := httptest.NewRecorder()
	if _, err := w.sessions.Start(context.Background(), rec, security.Subject{ID: who.ID, Tenant: who.Tenant}); err != nil {
		w.t.Fatal(err)
	}
	return rec.Result().Cookies()
}

// call makes a JSON request against the router.
func (w *world) call(method, target, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	w.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	defer func() {
		if recovered := recover(); recovered != nil {
			w.t.Fatalf("%s %s panicked, which a deployed router answers with 500: %v", method, target, recovered)
		}
	}()
	w.router.ServeHTTP(rec, req)
	return rec
}

var everyRoute = []struct{ method, target, body string }{
	{http.MethodGet, fcm.DefaultPrefix, ""},
	{http.MethodPost, fcm.DefaultPrefix, `{"token":"` + deviceToken + `","platform":"android"}`},
	{http.MethodPost, fcm.DefaultPrefix + "/unregister", `{"token":"` + deviceToken + `"}`},
	{http.MethodDelete, fcm.DefaultPrefix + "/device-1", ""},
}

func TestTheRoutesAreOffUnlessAskedFor(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	if n := len(w.router.Routes()); n != 0 {
		t.Fatalf("the module registered %d routes without being asked", n)
	}
}

func TestAVisitorWithNoSessionIsSentToSignIn(t *testing.T) {
	t.Parallel()
	w := routed(t)
	for _, r := range everyRoute {
		req := httptest.NewRequest(r.method, r.target, strings.NewReader(r.body))
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != middleware.SignInPath {
			t.Errorf("%s %s answered %d to %q, want %d to %s", r.method, r.target, rec.Code,
				rec.Header().Get("Location"), http.StatusSeeOther, middleware.SignInPath)
		}
	}
	if w.count(acme) != 0 {
		t.Fatal("a visitor wrote a row")
	}
}

func TestTheAppRegistersListsAndRemovesItsToken(t *testing.T) {
	t.Parallel()
	w := routed(t)
	ada := w.signIn(member(acme, "ada"))

	// The body names a tenant and a user, as a client that tries to choose
	// whose phone this is would. Neither is read.
	rec := w.call(http.MethodPost, fcm.DefaultPrefix,
		`{"token":"`+deviceToken+`","platform":"android","app":"com.example.app","locale":"pt-BR","tenant_id":"`+globex+`","user_id":"bob"}`, ada...)
	if rec.Code != http.StatusOK {
		t.Fatalf("register answered %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), deviceToken) {
		t.Fatalf("the response carries the token: %s", rec.Body)
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	created := envelope.Data
	if created["platform"] != "android" || created["user_id"] != "ada" || created["app"] != "com.example.app" {
		t.Fatalf("response = %v", created)
	}
	if w.count(acme) != 1 || w.count(globex) != 0 {
		t.Fatalf("rows: acme %d, globex %d: the tenant came from the body", w.count(acme), w.count(globex))
	}

	// Registering again is the same device.
	if rec := w.call(http.MethodPost, fcm.DefaultPrefix, `{"token":"`+deviceToken+`","platform":"android"}`, ada...); rec.Code != http.StatusOK {
		t.Fatalf("second register answered %d: %s", rec.Code, rec.Body)
	}
	if w.count(acme) != 1 {
		t.Fatalf("%d rows after registering twice", w.count(acme))
	}

	rec = w.call(http.MethodGet, fcm.DefaultPrefix, "", ada...)
	var list struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Data.Items) != 1 {
		t.Fatalf("list answered %d %s (%v)", rec.Code, rec.Body, err)
	}
	if strings.Contains(rec.Body.String(), deviceToken) || strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("the list carries a token: %s", rec.Body)
	}

	if rec := w.call(http.MethodPost, fcm.DefaultPrefix+"/unregister", `{"token":"`+deviceToken+`"}`, ada...); rec.Code != http.StatusNoContent {
		t.Fatalf("unregister answered %d: %s", rec.Code, rec.Body)
	}
	if w.count(acme) != 0 {
		t.Fatal("the device is still there")
	}
}

func TestTheAppRemovesADeviceByItsID(t *testing.T) {
	t.Parallel()
	w := routed(t)
	ada, bob := w.signIn(member(acme, "ada")), w.signIn(member(acme, "bob"))
	d := w.register(member(acme, "ada"), deviceToken, "android")

	if rec := w.call(http.MethodDelete, fcm.DefaultPrefix+"/"+d.ID, "", bob...); rec.Code != http.StatusNotFound {
		t.Fatalf("bob removing ada's phone answered %d, want 404", rec.Code)
	}
	if w.count(acme) != 1 {
		t.Fatal("bob removed ada's phone")
	}
	if rec := w.call(http.MethodDelete, fcm.DefaultPrefix+"/"+d.ID, "", ada...); rec.Code != http.StatusNoContent {
		t.Fatalf("ada removing her phone answered %d", rec.Code)
	}
	if rec := w.call(http.MethodDelete, fcm.DefaultPrefix+"/"+d.ID, "", ada...); rec.Code != http.StatusNotFound {
		t.Fatalf("removing it again answered %d, want 404", rec.Code)
	}
}

func TestARejectedRegistrationIsAnUnprocessableEntity(t *testing.T) {
	t.Parallel()
	w := routed(t)
	ada := w.signIn(member(acme, "ada"))

	rec := w.call(http.MethodPost, fcm.DefaultPrefix, `{"token":"","platform":"palm"}`, ada...)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("answered %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "token") || !strings.Contains(rec.Body.String(), "platform") {
		t.Fatalf("the answer names no field: %s", rec.Body)
	}
}

func TestASubjectWithoutTheActionGets403OnTheRoutes(t *testing.T) {
	t.Parallel()
	w := routed(t)
	w.resolve = func(who security.Subject) security.Subject {
		return security.Subject{ID: who.ID, Tenant: who.Tenant, Verified: true}
	}
	eve := w.signIn(security.Subject{ID: "eve", Tenant: acme})
	for _, r := range everyRoute {
		if rec := w.call(r.method, r.target, r.body, eve...); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s answered %d for a subject with no action, want 403", r.method, r.target, rec.Code)
		}
	}
}

func TestTheRoutesAreUnderThePrefixAndOutsideTheFrameworkNamespace(t *testing.T) {
	t.Parallel()
	w := install(t, false, func(c *fcm.Config) { c.Routes = true; c.Prefix = "/mobile/devices" })

	var got []string
	for _, route := range w.router.Routes() {
		if route.Module != "fcm" {
			t.Errorf("the route %s %s is not tagged with the module name: %q", route.Method, route.Pattern, route.Module)
		}
		if route.Pattern == reservedPrefix || strings.HasPrefix(route.Pattern, reservedPrefix+"/") {
			t.Errorf("the route %s %s is under %s/, which the framework refuses at boot", route.Method, route.Pattern, reservedPrefix)
		}
		got = append(got, route.Method+" "+route.Pattern)
	}
	sort.Strings(got)
	want := []string{
		"DELETE /mobile/devices/{id}", "GET /mobile/devices", "POST /mobile/devices", "POST /mobile/devices/unregister",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("registered %v, want %v", got, want)
	}
	if len(want) != len(everyRoute) {
		t.Fatalf("everyRoute reaches %d of %d routes", len(everyRoute), len(want))
	}
}

func TestNewRefusesAWiringThatCannotWork(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	sessions := security.NewSessionStore([]byte(appKey), 0, false, security.NewMemoryBackend())
	handle := data.Wrap(nil, data.DialectSQLite)
	valid := fcm.Config{Credentials: w.fb.credentials(t), Key: []byte(appKey)}

	if _, err := fcm.New(valid, handle, nil); err != nil {
		t.Fatalf("a module without routes needs no sessions: %v", err)
	}
	routes := valid
	routes.Routes = true
	if _, err := fcm.New(routes, handle, nil); err == nil {
		t.Error("routes without a session store were accepted")
	}
	if _, err := fcm.New(valid, nil, sessions); err == nil {
		t.Error("a nil database handle was accepted")
	}
	if _, err := fcm.New(fcm.Config{Key: []byte(appKey)}, handle, sessions); err == nil {
		t.Error("a configuration with no credentials and no sender was accepted")
	}
	if _, err := fcm.New(fcm.Config{Credentials: valid.Credentials, Key: []byte("short")}, handle, sessions); err == nil {
		t.Error("a short key was accepted")
	}
	// A Sender replaces the credentials, for the application's own tests.
	if _, err := fcm.New(fcm.Config{Sender: &fcm.Fake{}, Key: []byte(appKey)}, handle, sessions); err != nil {
		t.Errorf("a fake sender with no credentials was refused: %v", err)
	}
	for _, prefix := range []string{"fcm", "/fcm/", "/", "/widgets{", "/widgets/{id}", "/_arandu/fcm"} {
		bad := valid
		bad.Prefix = prefix
		if _, err := fcm.New(bad, handle, sessions); err == nil {
			t.Errorf("New accepted route prefix %q", prefix)
		}
	}
	for name, mutate := range map[string]func(*fcm.Config){
		"negative tries":     func(c *fcm.Config) { c.Tries = -1 },
		"too many phones":    func(c *fcm.Config) { c.MaxDevicesPerUser = fcm.MaxDevicesPerUserLimit + 1 },
		"negative stale":     func(c *fcm.Config) { c.StaleAfterDays = -1 },
		"plain http base":    func(c *fcm.Config) { c.APIBase = "http://fcm.example.com" },
		"relative base":      func(c *fcm.Config) { c.APIBase = "fcm.example.com" },
		"queue name, spaces": func(c *fcm.Config) { c.QueueName = "my queue" },
	} {
		bad := valid
		mutate(&bad)
		if _, err := fcm.New(bad, handle, sessions); err == nil {
			t.Errorf("New accepted %s", name)
		}
	}
}

func TestTheRouterAnswersAMissingDeviceWith404(t *testing.T) {
	t.Parallel()
	router := fhttp.NewRouter()
	router.Action(http.MethodGet, "/missing", func(*fhttp.Context) error { return fcm.ErrNotFound })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ErrNotFound answered %d, want 404", rec.Code)
	}
}

func TestRegisterJobHandlersRefusesAWorkerThatAlreadyHasIt(t *testing.T) {
	t.Parallel()
	w := install(t, true, nil)
	if err := w.module.RegisterJobHandlers(w.worker); err == nil {
		t.Error("registering the handler twice was accepted (the worker panics on a duplicate)")
	}
	if err := w.module.RegisterJobHandlers(nil); err == nil {
		t.Error("a nil worker was accepted")
	}
}
