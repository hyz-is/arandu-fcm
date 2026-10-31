package feature_test

import (
	"github.com/arandu-io/hesape/notifications"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	fcm "github.com/hyz-is/arandu-fcm"
)

// at walks a decoded JSON object by key and answers what is there, or nil.
func at(v any, path ...string) any {
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

func fullPush() fcm.Push {
	return fcm.NewPush().
		Title("Pedido enviado").
		Body("O pedido 42 saiu para entrega.").
		Image("https://cdn.example.com/orders/42.png").
		With("deeplink", "app://orders/42").
		With("order_id", "42").
		CollapseKey("order-42").
		TTL(time.Hour).
		ClickAction("OPEN_ORDER")
}

func TestAnAndroidPushCarriesTheAndroidBlock(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")

	if err := w.notify(acme, "ada", ping{fullPush()}); err != nil {
		t.Fatalf("sending: %v", err)
	}

	sent := w.fb.sent()
	if len(sent) != 1 {
		t.Fatalf("Firebase received %d messages, want 1", len(sent))
	}
	if got := sent[0].Header.Get("Authorization"); got != "Bearer "+w.fb.tokens[0] {
		t.Errorf("Authorization = %q, want the access token the exchange issued", got)
	}
	if got := sent[0].Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
	m := sent[0].message(t)
	want := map[string]any{
		"token":        deviceToken,
		"notification": map[string]any{"title": "Pedido enviado", "body": "O pedido 42 saiu para entrega."},
		"data":         map[string]any{"deeplink": "app://orders/42", "order_id": "42"},
		"android": map[string]any{
			"priority":     "HIGH",
			"collapse_key": "order-42",
			"ttl":          "3600s",
			"notification": map[string]any{"image": "https://cdn.example.com/orders/42.png", "click_action": "OPEN_ORDER"},
		},
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("the android message is\n%v\nwant\n%v", m, want)
	}
}

func TestAnIOSPushCarriesTheAPNSBlock(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), iosToken, "ios")

	if err := w.notify(acme, "ada", ping{fullPush()}); err != nil {
		t.Fatalf("sending: %v", err)
	}
	m := w.fb.sent()[0].message(t)

	expires := strconv.FormatInt(w.clock.now().Add(time.Hour).Unix(), 10)
	want := map[string]any{
		"token":        iosToken,
		"notification": map[string]any{"title": "Pedido enviado", "body": "O pedido 42 saiu para entrega."},
		"data":         map[string]any{"deeplink": "app://orders/42", "order_id": "42"},
		"apns": map[string]any{
			"headers": map[string]any{
				"apns-priority":    "10",
				"apns-expiration":  expires,
				"apns-collapse-id": "order-42",
			},
			"payload":     map[string]any{"aps": map[string]any{"sound": "default", "mutable-content": float64(1)}},
			"fcm_options": map[string]any{"image": "https://cdn.example.com/orders/42.png"},
		},
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("the ios message is\n%v\nwant\n%v", m, want)
	}
	if _, android := m["android"]; android {
		t.Fatal("an iOS message carries the android block")
	}
}

func TestMutableContentIsOnlyForAnImage(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), iosToken, "ios")

	plain := fcm.NewPush().Title("Oi").Body("Sem imagem")
	if err := w.notify(acme, "ada", ping{plain}); err != nil {
		t.Fatal(err)
	}
	m := w.fb.sent()[0].message(t)
	aps, _ := at(m, "apns", "payload", "aps").(map[string]any)
	if _, has := aps["mutable-content"]; has {
		t.Errorf("aps = %v: mutable-content is for an image only", aps)
	}
	if aps["sound"] != "default" {
		t.Errorf("aps = %v, want the default sound", aps)
	}
	if at(m, "apns", "fcm_options") != nil {
		t.Errorf("fcm_options present without an image: %v", at(m, "apns", "fcm_options"))
	}
}

func TestANormalPushLowersThePriorityOnBothPlatforms(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.register(member(acme, "ada"), iosToken, "ios")

	if err := w.notify(acme, "ada", ping{fcm.NewPush().Title("t").Body("b").Normal()}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, s := range w.fb.sent() {
		m := s.message(t)
		switch {
		case at(m, "android") != nil:
			seen["android"], _ = at(m, "android", "priority").(string)
		case at(m, "apns") != nil:
			seen["ios"], _ = at(m, "apns", "headers", "apns-priority").(string)
		}
	}
	if seen["android"] != "NORMAL" || seen["ios"] != "5" {
		t.Fatalf("priorities = %v, want android NORMAL and ios 5", seen)
	}
}

func TestADataOnlyPushHasNoAlert(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.register(member(acme, "ada"), iosToken, "ios")

	if err := w.notify(acme, "ada", ping{fcm.NewPush().With("sync", "orders")}); err != nil {
		t.Fatal(err)
	}
	for _, s := range w.fb.sent() {
		m := s.message(t)
		if _, alert := m["notification"]; alert {
			t.Errorf("a data-only push carries a notification: %v", m)
		}
		if at(m, "data", "sync") != "orders" {
			t.Errorf("data = %v", m["data"])
		}
		if at(m, "apns") != nil {
			if at(m, "apns", "headers", "apns-push-type") != "background" || at(m, "apns", "headers", "apns-priority") != "5" ||
				at(m, "apns", "payload", "aps", "content-available") != float64(1) {
				t.Errorf("the silent iOS push is %v", m["apns"])
			}
		}
	}
}

func TestEveryPhoneOfTheRecipientIsReached(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.register(member(acme, "ada"), iosToken, "ios")
	w.register(member(acme, "bob"), "tok-CCCC3333:APA91bH-registration-token-of-bob", "android")

	if err := w.notify(acme, "ada", ping{fcm.NewPush().Title("t").Body("b")}); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]bool{}
	for _, s := range w.fb.sent() {
		tokens[s.message(t)["token"].(string)] = true
	}
	if len(tokens) != 2 || !tokens[deviceToken] || !tokens[iosToken] {
		t.Fatalf("tokens reached = %v, want ada's two and not bob's", tokens)
	}
}

func TestARecipientWithNoPhoneIsNotAFailure(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)

	// The Notifier treats ErrNotAddressed as "not reachable this way": the
	// notification's other channels carry on and nothing is reported.
	if err := w.notify(acme, "nobody", ping{fcm.NewPush().Title("t")}); err != nil {
		t.Fatalf("a user with no phone failed the notification: %v", err)
	}
	if n := len(w.fb.sent()); n != 0 {
		t.Fatalf("Firebase received %d messages", n)
	}
}

func TestAnInvalidPushIsRefusedBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")

	for name, push := range map[string]fcm.Push{
		"empty":           fcm.NewPush(),
		"http image":      fcm.NewPush().Title("t").Image("http://example.com/a.png"),
		"reserved key":    fcm.NewPush().Title("t").With("google.foo", "x"),
		"ttl too long":    fcm.NewPush().Title("t").TTL(30 * 24 * time.Hour),
		"payload too big": fcm.NewPush().Title("t").With("blob", strings.Repeat("x", 5000)),
	} {
		if err := w.notify(acme, "ada", ping{push}); err == nil {
			t.Errorf("%s: the push was accepted", name)
		}
	}
	if n := len(w.fb.sent()); n != 0 {
		t.Fatalf("Firebase received %d messages for pushes that are not valid", n)
	}
}

// localized words its push per language.
type localized struct{ ping }

func (l localized) ToPushIn(_ notifications.Notifiable, locale string) fcm.Push {
	if strings.HasPrefix(locale, "en") {
		return fcm.NewPush().Title("Order shipped").Body("On its way.")
	}
	return fcm.NewPush().Title("Pedido enviado").Body("A caminho.")
}

func TestAPushIsWordedInTheLanguageOfEachPhone(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	ada := member(acme, "ada")
	if _, err := w.module.Devices().Register(t.Context(), ada, fcm.RegisterRequest{Token: deviceToken, Platform: "android", Locale: "en_US"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.module.Devices().Register(t.Context(), ada, fcm.RegisterRequest{Token: iosToken, Platform: "ios", Locale: "pt-BR"}); err != nil {
		t.Fatal(err)
	}

	if err := w.notify(acme, "ada", localized{ping{fcm.NewPush().Title("fallback")}}); err != nil {
		t.Fatal(err)
	}
	titles := map[string]string{}
	for _, s := range w.fb.sent() {
		m := s.message(t)
		titles[m["token"].(string)], _ = at(m, "notification", "title").(string)
	}
	if titles[deviceToken] != "Order shipped" || titles[iosToken] != "Pedido enviado" {
		t.Fatalf("titles = %v", titles)
	}
}

func TestTheAccessTokenIsCachedUntilItIsAboutToExpire(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	push := ping{fcm.NewPush().Title("t").Body("b")}

	for range 3 {
		if err := w.notify(acme, "ada", push); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.fb.tokenRequests(); n != 1 {
		t.Fatalf("the token endpoint was called %d times for 3 sends, want 1", n)
	}

	// 50 minutes in: ten minutes left, outside the five minute margin, so the
	// token is still the cached one.
	w.clock.advance(50 * time.Minute)
	if err := w.notify(acme, "ada", push); err != nil {
		t.Fatal(err)
	}
	if n := w.fb.tokenRequests(); n != 1 {
		t.Fatalf("the token was exchanged again with 10 minutes left: %d calls", n)
	}

	// 57 minutes in: three minutes left, inside the margin.
	w.clock.advance(7 * time.Minute)
	if err := w.notify(acme, "ada", push); err != nil {
		t.Fatal(err)
	}
	if n := w.fb.tokenRequests(); n != 2 {
		t.Fatalf("the token was not renewed before it expired: %d calls", n)
	}
	sent := w.fb.sent()
	if got := sent[len(sent)-1].Header.Get("Authorization"); got != "Bearer "+w.fb.tokens[1] {
		t.Fatalf("the last send used %q, want the renewed token", got)
	}
	if bad := w.fb.problems(); len(bad) != 0 {
		t.Fatalf("the assertions were not what Google accepts: %v", bad)
	}
}

func TestARefusedAccessTokenIsReplacedOnceAndTheSendRepeated(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.fb.script = func(n int, _ sentMessage) *reply {
		if n == 1 {
			return &reply{Status: 401, Body: `{"error":{"code":401,"message":"expired","status":"UNAUTHENTICATED"}}`}
		}
		return nil
	}

	if err := w.notify(acme, "ada", ping{fcm.NewPush().Title("t").Body("b")}); err != nil {
		t.Fatalf("a revoked access token failed the send: %v", err)
	}
	if n := len(w.fb.sent()); n != 2 {
		t.Fatalf("Firebase received %d sends, want 2: the refused one and its repeat", n)
	}
	if n := w.fb.tokenRequests(); n != 2 {
		t.Fatalf("the token endpoint was called %d times, want 2", n)
	}
}

func TestAnAccessTokenRefusedTwiceIsNotRepeatedAgain(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.fb.script = func(int, sentMessage) *reply {
		return &reply{Status: 401, Body: `{"error":{"code":401,"message":"no","status":"UNAUTHENTICATED"}}`}
	}

	if err := w.notify(acme, "ada", ping{fcm.NewPush().Title("t").Body("b")}); err == nil {
		t.Fatal("a refused credential reported success")
	}
	if n := len(w.fb.sent()); n != 2 {
		t.Fatalf("Firebase received %d sends, want exactly 2 and no loop", n)
	}
}
