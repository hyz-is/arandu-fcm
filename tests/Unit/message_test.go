package unit_test

import (
	"strings"
	"testing"
	"time"

	fcm "github.com/hyz-is/arandu-fcm"
)

func TestAPushIsValidatedBeforeItIsSent(t *testing.T) {
	t.Parallel()

	valid := fcm.NewPush().Title("Oi").Body("Tudo bem?").Image("https://cdn.example.com/a.png").
		With("deeplink", "app://x").CollapseKey("k").TTL(time.Hour).ClickAction("OPEN")
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid push was refused: %v", err)
	}
	if err := fcm.NewPush().With("sync", "1").Validate(); err != nil {
		t.Fatalf("a data-only push was refused: %v", err)
	}

	for name, push := range map[string]fcm.Push{
		"nothing at all":   fcm.NewPush(),
		"blank alert":      fcm.NewPush().Title("  ").Body(""),
		"title too long":   fcm.NewPush().Title(strings.Repeat("x", fcm.MaxTitleRunes+1)),
		"body too long":    fcm.NewPush().Body(strings.Repeat("x", fcm.MaxBodyRunes+1)),
		"http image":       fcm.NewPush().Title("t").Image("http://example.com/a.png"),
		"relative image":   fcm.NewPush().Title("t").Image("/a.png"),
		"unknown priority": fcm.NewPush().Title("t").Priority("urgent"),
		"collapse key":     fcm.NewPush().Title("t").CollapseKey(strings.Repeat("k", fcm.MaxCollapseKeyBytes+1)),
		"negative ttl":     fcm.NewPush().Title("t").TTL(-time.Second),
		"ttl past 4 weeks": fcm.NewPush().Title("t").TTL(fcm.MaxTTL + time.Second),
		"empty data key":   fcm.NewPush().Title("t").With("", "x"),
		"reserved from":    fcm.NewPush().Title("t").With("from", "x"),
		"reserved google":  fcm.NewPush().Title("t").With("google.sent_time", "x"),
		"reserved gcm":     fcm.NewPush().Title("t").With("gcm.notification.x", "x"),
		"key with space":   fcm.NewPush().Title("t").With("a b", "x"),
		"payload too big":  fcm.NewPush().Title("t").With("blob", strings.Repeat("x", fcm.MaxPayloadBytes)),
	} {
		if err := push.Validate(); err == nil {
			t.Errorf("%s: the push was accepted", name)
		}
	}

	many := fcm.NewPush().Title("t")
	for i := range fcm.MaxDataKeys + 1 {
		many = many.With("k"+strings.Repeat("a", i), "v")
	}
	if err := many.Validate(); err == nil {
		t.Error("a push with too many data pairs was accepted")
	}
}

func TestThePushSettersDoNotShareState(t *testing.T) {
	t.Parallel()

	base := fcm.NewPush().Title("t").With("a", "1")
	derived := base.With("b", "2").Title("other")
	if len(base.DataValues) != 1 || base.TitleText != "t" {
		t.Fatalf("building a push from another changed it: %+v", base)
	}
	if derived.DataValues["a"] != "1" || derived.DataValues["b"] != "2" {
		t.Fatalf("the derived push lost data: %+v", derived)
	}
	given := map[string]string{"x": "1"}
	pushed := fcm.NewPush().Data(given)
	given["x"] = "changed"
	if pushed.DataValues["x"] != "1" {
		t.Fatal("Data keeps the caller's map")
	}
}

func TestThePriorityIsHighUnlessAskedOtherwise(t *testing.T) {
	t.Parallel()

	if fcm.NewPush().Urgency() != fcm.PriorityHigh {
		t.Error("the default priority is not high")
	}
	if fcm.NewPush().Normal().Urgency() != fcm.PriorityNormal {
		t.Error("Normal did not lower the priority")
	}
	if !fcm.NewPush().With("a", "1").Silent() || fcm.NewPush().Title("t").Silent() {
		t.Error("Silent does not tell a data-only push from an alert")
	}
}
