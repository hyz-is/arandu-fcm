package feature_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arandu-io/hesape/queue"

	fcm "github.com/hyz-is/arandu-fcm"
)

func shown() fcm.Push { return fcm.NewPush().Title("t").Body("b") }

func TestAnExpiredTokenRemovesTheDevice(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"UNREGISTERED":                   unregisteredBody,
		"INVALID_ARGUMENT about a token": badTokenBody,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := install(t, false, nil)
			w.register(member(acme, "ada"), deviceToken, "android")
			w.register(member(acme, "ada"), iosToken, "ios")
			w.fb.script = func(_ int, m sentMessage) *reply {
				if m.message(t)["token"] == deviceToken {
					status := 404
					if strings.Contains(body, "INVALID_ARGUMENT") {
						status = 400
					}
					return &reply{Status: status, Body: body}
				}
				return nil
			}

			// The dead phone is not a failure of the notification: the other
			// phone of the user is still reached.
			if err := w.notify(acme, "ada", ping{shown()}); err != nil {
				t.Fatalf("a dead token failed the notification: %v", err)
			}
			left := w.devices(acme, "ada")
			if len(left) != 1 || left[0].Platform != fcm.PlatformIOS {
				t.Fatalf("devices left = %v, want only the iPhone", left)
			}
			if !slices.Contains(w.events(), fcm.EventDeviceRemoved) {
				t.Fatalf("the removal is not in the outbox: %v", w.events())
			}

			// And the dead token is never sent to again.
			before := len(w.fb.sent())
			if err := w.notify(acme, "ada", ping{shown()}); err != nil {
				t.Fatal(err)
			}
			if got := len(w.fb.sent()) - before; got != 1 {
				t.Fatalf("the second notification made %d sends, want 1 (the iPhone)", got)
			}
		})
	}
}

func TestAnInvalidPayloadDoesNotCostTheUserAPhone(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.fb.script = func(int, sentMessage) *reply { return &reply{Status: 400, Body: badPayloadBody} }

	err := w.notify(acme, "ada", ping{shown()})
	if err == nil {
		t.Fatal("a refused message reported success")
	}
	var failure *fcm.SendError
	if !errors.As(err, &failure) || failure.Code != "INVALID_ARGUMENT" || failure.InvalidToken || failure.Retryable() {
		t.Fatalf("the failure is %#v, want INVALID_ARGUMENT, not about the token, not retryable", failure)
	}
	if n := len(w.devices(acme, "ada")); n != 1 {
		t.Fatalf("%d devices left, want the phone kept", n)
	}
	if n := len(w.fb.sent()); n != 1 {
		t.Fatalf("a refused message was sent %d times", n)
	}
}

func TestWithoutAQueueAThrottledPushIsReportedAndNeverRepeated(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.fb.script = func(int, sentMessage) *reply {
		return &reply{Status: 429, Header: map[string]string{"Retry-After": "30"}, Body: quotaBody}
	}

	err := w.notify(acme, "ada", ping{shown()})
	var failure *fcm.SendError
	if !errors.As(err, &failure) || !failure.Retryable() || failure.RetryAfter != 30*time.Second {
		t.Fatalf("err = %v, want a retryable failure asking for 30s", err)
	}
	if n := len(w.fb.sent()); n != 1 {
		t.Fatalf("Firebase received %d sends, want 1: nothing retries without a queue", n)
	}
}

// pushedAt reads when the only job on the queue becomes eligible.
func (w *world) eligibleAt() time.Time {
	w.t.Helper()
	var value any
	if err := w.db.QueryRowContext(context.Background(), "SELECT run_at FROM jobs WHERE failed_at IS NULL").Scan(&value); err != nil {
		w.t.Fatal(err)
	}
	switch v := value.(type) {
	case time.Time:
		return v
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
			if at, err := time.Parse(layout, v); err == nil {
				return at
			}
		}
	}
	w.t.Fatalf("run_at is %v (%T)", value, value)
	return time.Time{}
}

func TestAThrottledPushIsRescheduledAfterTheRetryAfter(t *testing.T) {
	t.Parallel()
	w := install(t, true, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.fb.script = func(n int, _ sentMessage) *reply {
		if n == 1 {
			return &reply{Status: 429, Header: map[string]string{"Retry-After": "120"}, Body: quotaBody}
		}
		return nil
	}

	if err := w.notify(acme, "ada", ping{shown()}); err != nil {
		t.Fatalf("queueing: %v", err)
	}
	if w.size() != 1 || len(w.fb.sent()) != 0 {
		t.Fatalf("queued %d jobs and sent %d: the channel must only queue", w.size(), len(w.fb.sent()))
	}

	if err := w.runJob(); err == nil {
		t.Fatal("the throttled delivery reported success to the worker")
	}
	if len(w.fb.sent()) != 1 {
		t.Fatalf("Firebase received %d sends", len(w.fb.sent()))
	}
	if w.size() != 1 {
		t.Fatalf("the job is not waiting for later: size %d", w.size())
	}
	// The first backoff is 10s; the 120s Retry-After holds it longer.
	if wait := time.Until(w.eligibleAt()); wait < 110*time.Second || wait > 125*time.Second {
		t.Fatalf("the job is eligible in %s, want about the 120s Firebase asked for", wait)
	}

	// Running the worker again now sends nothing: the job is not due.
	if err := w.runJob(); err != nil {
		t.Fatalf("an idle worker failed: %v", err)
	}
	if len(w.fb.sent()) != 1 {
		t.Fatalf("a push that is not due was sent: %d sends", len(w.fb.sent()))
	}
}

func TestAnUnavailableServiceIsRetriedWithBackoffAndThenParked(t *testing.T) {
	t.Parallel()
	w := install(t, true, func(c *fcm.Config) { c.Tries = 2 })
	w.register(member(acme, "ada"), deviceToken, "android")
	w.fb.script = func(int, sentMessage) *reply { return &reply{Status: 503, Body: unavailableBody} }

	if err := w.notify(acme, "ada", ping{shown()}); err != nil {
		t.Fatal(err)
	}
	if err := w.runJob(); err == nil {
		t.Fatal("a 503 reported success")
	}
	if w.size() != 1 || w.failedJobs() != 0 {
		t.Fatalf("after the first failure: size %d, parked %d; want it waiting for a retry", w.size(), w.failedJobs())
	}
	// Bring the retry due and run it: the second and last try.
	if _, err := w.db.ExecContext(context.Background(), "UPDATE jobs SET run_at = ?", time.Now().Add(-time.Minute).UTC()); err != nil {
		t.Fatal(err)
	}
	if err := w.runJob(); err == nil {
		t.Fatal("the last try reported success")
	}
	if w.failedJobs() != 1 {
		t.Fatalf("the job was not parked after its tries: parked %d", w.failedJobs())
	}
	// Parked work is not retried: another worker pass sends nothing.
	if err := w.runJob(); err != nil {
		t.Fatal(err)
	}
	if n := len(w.fb.sent()); n != 2 {
		t.Fatalf("Firebase received %d sends, want exactly the 2 tries", n)
	}
}

func TestARefusedMessageIsParkedAtOnceAndNotRetried(t *testing.T) {
	t.Parallel()
	w := install(t, true, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.fb.script = func(int, sentMessage) *reply { return &reply{Status: 400, Body: badPayloadBody} }

	if err := w.notify(acme, "ada", ping{shown()}); err != nil {
		t.Fatal(err)
	}
	err := w.runJob()
	if !errors.Is(err, queue.ErrManuallyFailed) {
		t.Fatalf("the worker saw %v, want a manual failure", err)
	}
	if w.failedJobs() != 1 {
		t.Fatal("a message Firebase refuses was not parked")
	}
	if n := len(w.fb.sent()); n != 1 {
		t.Fatalf("the refused message was sent %d times", n)
	}
}

func TestAQueuedPushToADeadTokenRemovesTheDevice(t *testing.T) {
	t.Parallel()
	w := install(t, true, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.fb.script = func(int, sentMessage) *reply { return &reply{Status: 404, Body: unregisteredBody} }

	if err := w.notify(acme, "ada", ping{shown()}); err != nil {
		t.Fatal(err)
	}
	if err := w.runJob(); err != nil {
		t.Fatalf("the worker failed on a dead token: %v", err)
	}
	if w.size() != 0 || w.failedJobs() != 0 {
		t.Fatalf("the job is not done: size %d, parked %d", w.size(), w.failedJobs())
	}
	if n := len(w.devices(acme, "ada")); n != 0 {
		t.Fatalf("%d devices left, want none", n)
	}
}

func TestAQueuedPushToADeviceRemovedMeanwhileIsDropped(t *testing.T) {
	t.Parallel()
	w := install(t, true, nil)
	d := w.register(member(acme, "ada"), deviceToken, "android")
	if err := w.notify(acme, "ada", ping{shown()}); err != nil {
		t.Fatal(err)
	}
	if err := w.module.Devices().UnregisterID(context.Background(), member(acme, "ada"), d.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.runJob(); err != nil {
		t.Fatalf("a push for a removed device failed the worker: %v", err)
	}
	if len(w.fb.sent()) != 0 || w.size() != 0 {
		t.Fatalf("sent %d, queued %d", len(w.fb.sent()), w.size())
	}
}

func TestTheJobCarriesNoTokenAndTheTimeItExpires(t *testing.T) {
	t.Parallel()
	w := install(t, true, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	if err := w.notify(acme, "ada", ping{shown().TTL(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var payload, attributes string
	if err := w.db.QueryRowContext(context.Background(), "SELECT payload, attributes FROM jobs").Scan(&payload, &attributes); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload+attributes, deviceToken) {
		t.Fatal("the registration token is in the job")
	}
	if !strings.Contains(attributes, `"maxTries":5`) || strings.Contains(attributes, "retryUntil") {
		t.Fatalf("attributes = %s: want the default tries and no deadline that would replace them", attributes)
	}
	if !strings.Contains(payload, "expires_at") {
		t.Fatalf("payload = %s: want the time the push stops being worth sending", payload)
	}
}

func TestTheCredentialNeverAppearsInALogOrAnError(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")

	// The token endpoint answers an error that echoes the assertion it was
	// given, and the send endpoint one that echoes the registration token.
	w.fb.tokenScript = func(n int) *reply {
		if n == 1 {
			return &reply{Status: 503, Body: `{"error":"backend_error","error_description":"try again"}`}
		}
		return nil
	}
	var errs []string
	for range 2 {
		if err := w.notify(acme, "ada", ping{shown()}); err != nil {
			errs = append(errs, err.Error())
		}
	}
	w.fb.script = func(_ int, m sentMessage) *reply {
		token := m.message(t)["token"].(string)
		return &reply{Status: 400, Body: `{"error":{"code":400,"message":"bad ` + token + ` with ` + w.fb.tokens[0] + `","status":"INVALID_ARGUMENT"}}`}
	}
	if err := w.notify(acme, "ada", ping{shown()}); err != nil {
		errs = append(errs, err.Error())
	}
	// A token request refused with the assertion echoed back.
	w.clock.advance(2 * time.Hour)
	w.fb.mu.Lock()
	w.fb.tokenScript = func(int) *reply {
		return &reply{Status: 400, Body: `{"error":"invalid_grant","error_description":"Invalid JWT: ` + strings.Repeat("A", 20) + `"}`}
	}
	w.fb.mu.Unlock()
	if err := w.notify(acme, "ada", ping{shown()}); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) == 0 {
		t.Fatal("none of the failures produced an error, so this test proved nothing")
	}

	_, pemKey := serviceKey(t)
	body := strings.TrimSpace(strings.Split(pemKey, "\n")[1])
	secrets := append([]string{deviceToken, body, w.fb.keyID}, w.fb.tokens...)
	surface := strings.Join(errs, "\n") + "\n" + w.logs.String()
	for _, secret := range secrets {
		if strings.Contains(surface, secret) {
			t.Errorf("a secret leaked into an error or a log line: %q\n%s", secret, surface)
		}
	}
	if w.logs.String() == "" {
		t.Fatal("nothing was logged, so the check of the log proved nothing")
	}
}

func TestARejectedExchangeThatEchoesTheAssertionIsScrubbed(t *testing.T) {
	t.Parallel()
	_, pemKey := serviceKey(t)
	keyBody := strings.Join(strings.Split(strings.TrimSpace(pemKey), "\n")[1:3], "")

	var echoed string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		echoed = r.Form.Get("assertion")
		body, _ := json.Marshal(map[string]string{
			"error":             "invalid_grant",
			"error_description": "Invalid JWT " + echoed + " key " + keyBody,
		})
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	raw, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "demo", "private_key": pemKey,
		"client_email": "push@demo.iam.gserviceaccount.com", "token_uri": server.URL,
	})
	credentials, err := fcm.ParseCredentials(raw)
	if err != nil {
		t.Fatal(err)
	}
	client, err := fcm.NewClient(fcm.ClientConfig{Credentials: credentials, APIBase: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Send(context.Background(), fcm.Outgoing{Token: deviceToken, Platform: fcm.PlatformAndroid, Push: shown()})
	var failure *fcm.SendError
	if !errors.As(err, &failure) || failure.Code != "invalid_grant" || failure.Retryable() {
		t.Fatalf("err = %v, want a permanent invalid_grant", err)
	}
	if echoed == "" {
		t.Fatal("the endpoint never saw an assertion")
	}
	if strings.Contains(err.Error(), echoed) || strings.Contains(err.Error(), keyBody) {
		t.Fatalf("the error repeats a secret the endpoint echoed: %v", err)
	}
}

func TestTheRegistrationTokenIsSealedAtRest(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")

	var sealed, hash string
	if err := w.db.QueryRowContext(context.Background(), "SELECT token, token_hash FROM fcm_devices").Scan(&sealed, &hash); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, deviceToken) || strings.Contains(hash, deviceToken) || sealed == "" || len(hash) != 64 {
		t.Fatalf("the token is not sealed: token column %q, hash %q", sealed, hash)
	}

	// A sealed value moved into another row does not open there.
	d := w.devices(acme, "ada")[0]
	if _, err := w.db.ExecContext(context.Background(), "UPDATE fcm_devices SET id = 'moved' WHERE id = ?", d.ID); err != nil {
		t.Fatal(err)
	}
	err := w.notify(acme, "ada", ping{shown()})
	if err == nil || len(w.fb.sent()) != 0 {
		t.Fatalf("a token sealed for another row was sent: err %v, sends %d", err, len(w.fb.sent()))
	}
}

func TestAQueuedPushPastItsTTLIsDroppedNotSent(t *testing.T) {
	t.Parallel()
	w := install(t, true, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	if err := w.notify(acme, "ada", ping{shown().TTL(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	w.clock.advance(2 * time.Minute)

	if err := w.runJob(); err != nil {
		t.Fatalf("an expired push failed the worker: %v", err)
	}
	if len(w.fb.sent()) != 0 || w.size() != 0 {
		t.Fatalf("an expired push was sent (%d) or kept (%d)", len(w.fb.sent()), w.size())
	}
}
