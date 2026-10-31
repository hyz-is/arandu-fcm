package feature_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arandu-io/framework/security"
	"github.com/arandu-io/hesape/database/model"

	fcm "github.com/hyz-is/arandu-fcm"
)

func TestRegisteringATokenTwiceIsOneDevice(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	ada := member(acme, "ada")

	first := w.register(ada, deviceToken, "android")
	w.clock.advance(time.Hour)
	second, err := w.module.Devices().Register(context.Background(), ada, fcm.RegisterRequest{
		Token: deviceToken, Platform: "ANDROID", App: "com.example.pro", Locale: "en_US",
	})
	if err != nil {
		t.Fatal(err)
	}

	if first.ID != second.ID || w.count(acme) != 1 {
		t.Fatalf("ids %s and %s, %d rows: want one device", first.ID, second.ID, w.count(acme))
	}
	if second.App != "com.example.pro" || second.Locale != "en-US" || second.Platform != fcm.PlatformAndroid {
		t.Fatalf("the second registration did not refresh the device: %v", second)
	}
	if !second.LastSeenAt.After(first.CreatedAt) {
		t.Fatalf("last seen %s did not move past creation %s", second.LastSeenAt, first.CreatedAt)
	}
	if !second.CreatedAt.Equal(first.CreatedAt.Truncate(time.Second)) {
		t.Fatalf("registering again changed the creation time: %s -> %s", first.CreatedAt, second.CreatedAt)
	}
}

func TestATokenChangesHandsWhenAnotherUserRegistersIt(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")

	// The phone is signed into bob's account now: the same token replaces the
	// earlier registration, and ada stops receiving bob's pushes.
	w.register(member(acme, "bob"), deviceToken, "android")

	if n := w.count(acme); n != 1 {
		t.Fatalf("%d rows, want the one device to have changed hands", n)
	}
	if got := len(w.devices(acme, "ada")); got != 0 {
		t.Fatalf("ada still holds %d devices", got)
	}
	if got := len(w.devices(acme, "bob")); got != 1 {
		t.Fatalf("bob holds %d devices", got)
	}

	if err := w.notify(acme, "ada", ping{shown()}); err != nil || len(w.fb.sent()) != 0 {
		t.Fatalf("ada was pushed to a phone that is bob's now: err %v, sends %d", err, len(w.fb.sent()))
	}
	events := w.events()
	if !slices.Contains(events, fcm.EventDeviceRegistered) {
		t.Fatalf("events = %v", events)
	}
}

func TestADeviceOfAnotherTenantIsNeitherListedNorRemovedNorPushed(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	theirs := w.register(member(globex, "ada"), deviceToken, "android")
	w.register(member(acme, "ada"), iosToken, "ios")

	// Listing: acme's administrator reads acme's ada, never globex's.
	list := w.devices(acme, "ada")
	if len(list) != 1 || list[0].Platform != fcm.PlatformIOS {
		t.Fatalf("acme's administrator sees %v", list)
	}

	// Removing by id: another tenant's device is not found.
	err := w.module.Devices().UnregisterID(context.Background(), member(acme, "admin-acme"), theirs.ID)
	if !errors.Is(err, fcm.ErrNotFound) || !errors.Is(err, model.ErrModelNotFound) {
		t.Fatalf("removing another tenant's device answered %v, want not found", err)
	}
	// Removing by token: nothing of acme's holds it.
	if err := w.module.Devices().Unregister(context.Background(), member(acme, "admin-acme"), deviceToken); err != nil {
		t.Fatal(err)
	}
	if w.count(globex) != 1 {
		t.Fatal("a device of another tenant was removed")
	}

	// Pushing: acme's notification to a user id both tenants share reaches only
	// acme's phone.
	if err := w.notify(acme, "ada", ping{shown()}); err != nil {
		t.Fatal(err)
	}
	sent := w.fb.sent()
	if len(sent) != 1 || sent[0].message(t)["token"] != iosToken {
		t.Fatalf("sent %d messages, want only acme's phone", len(sent))
	}
}

func TestTheSameTokenInTwoTenantsIsTwoDevices(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	w.register(member(globex, "ada"), deviceToken, "android")

	if w.count(acme) != 1 || w.count(globex) != 1 {
		t.Fatalf("rows: acme %d, globex %d", w.count(acme), w.count(globex))
	}
	var a, g string
	if err := w.db.QueryRowContext(context.Background(), "SELECT token_hash FROM fcm_devices WHERE tenant_id = ?", acme).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := w.db.QueryRowContext(context.Background(), "SELECT token_hash FROM fcm_devices WHERE tenant_id = ?", globex).Scan(&g); err != nil {
		t.Fatal(err)
	}
	if a == g {
		t.Fatal("the same token digests to the same value in two tenants, which would correlate them")
	}
}

func TestAUserSeesOnlyTheirOwnPhonesUnlessTheyManage(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	w.register(member(acme, "ada"), deviceToken, "android")
	bobs := w.register(member(acme, "bob"), iosToken, "ios")

	ctx := context.Background()
	own, err := w.module.Devices().ListForUser(ctx, member(acme, "ada"), "")
	if err != nil || len(own) != 1 {
		t.Fatalf("own list = %v, %v", own, err)
	}
	if _, err := w.module.Devices().ListForUser(ctx, member(acme, "ada"), "bob"); !errors.Is(err, security.ErrForbidden) {
		t.Fatalf("reading another user's phones answered %v, want forbidden", err)
	}
	if err := w.module.Devices().UnregisterID(ctx, member(acme, "ada"), bobs.ID); !errors.Is(err, fcm.ErrNotFound) {
		t.Fatalf("removing another user's phone answered %v, want not found", err)
	}
	// And by token: it is not ada's, so there is nothing to remove, and bob's
	// phone stays.
	if err := w.module.Devices().Unregister(ctx, member(acme, "ada"), iosToken); err != nil {
		t.Fatal(err)
	}
	if len(w.devices(acme, "bob")) != 1 {
		t.Fatal("ada removed bob's phone")
	}
	// The administrator may.
	if err := w.module.Devices().UnregisterID(ctx, member(acme, "admin-acme"), bobs.ID); err != nil {
		t.Fatal(err)
	}
	if len(w.devices(acme, "bob")) != 0 {
		t.Fatal("the administrator could not remove a phone of the tenant")
	}
}

func TestUnregisteringIsIdempotent(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	ada := member(acme, "ada")
	w.register(ada, deviceToken, "android")

	for range 2 {
		if err := w.module.Devices().Unregister(context.Background(), ada, deviceToken); err != nil {
			t.Fatalf("unregistering: %v", err)
		}
	}
	if w.count(acme) != 0 {
		t.Fatal("the device is still there")
	}
	got := w.events()
	if n := strings.Count(strings.Join(got, ","), fcm.EventDeviceUnregistered); n != 1 {
		t.Fatalf("events = %v: want one unregistration recorded", got)
	}
}

func TestASubjectWithoutTheActionIsRefused(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	nobody := security.Subject{ID: "eve", Tenant: acme, Verified: true}

	_, err := w.module.Devices().Register(context.Background(), nobody, fcm.RegisterRequest{Token: deviceToken, Platform: "android"})
	if !errors.Is(err, security.ErrForbidden) {
		t.Fatalf("Register answered %v, want forbidden", err)
	}
	if w.count(acme) != 0 {
		t.Fatal("a refused registration wrote a row")
	}
}

func TestTheOwnerAndTheTenantAreNeverTheRequests(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)

	d := w.register(member(acme, "ada"), deviceToken, "android")
	if d.UserID != "ada" || d.TenantID != acme {
		t.Fatalf("the device is %s/%s, want the subject's and the Grant's", d.TenantID, d.UserID)
	}
}

func TestAUserKeepsAtMostTheConfiguredNumberOfPhones(t *testing.T) {
	t.Parallel()
	w := install(t, false, func(c *fcm.Config) { c.MaxDevicesPerUser = 2 })
	ada := member(acme, "ada")

	w.register(ada, "tok-0001-aaaaaaaaaaaaaaaa", "android")
	w.clock.advance(time.Minute)
	w.register(ada, "tok-0002-bbbbbbbbbbbbbbbb", "ios")
	w.clock.advance(time.Minute)
	w.register(ada, "tok-0001-aaaaaaaaaaaaaaaa", "android") // seen again: the second is now the oldest
	w.clock.advance(time.Minute)
	w.register(ada, "tok-0003-cccccccccccccccc", "android")

	list := w.devices(acme, "ada")
	if len(list) != 2 {
		t.Fatalf("%d phones kept, want 2", len(list))
	}
	if err := w.notify(acme, "ada", ping{shown()}); err != nil {
		t.Fatal(err)
	}
	tokens := []string{}
	for _, s := range w.fb.sent() {
		tokens = append(tokens, s.message(t)["token"].(string))
	}
	slices.Sort(tokens)
	if !slices.Equal(tokens, []string{"tok-0001-aaaaaaaaaaaaaaaa", "tok-0003-cccccccccccccccc"}) {
		t.Fatalf("pushed to %v: the phone seen least recently should be the one that went", tokens)
	}
	if !slices.Contains(w.events(), fcm.EventDeviceRemoved) {
		t.Fatalf("the removal is not in the outbox: %v", w.events())
	}
}

func TestRegisteringValidatesTheInput(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	ada := member(acme, "ada")

	for name, in := range map[string]fcm.RegisterRequest{
		"no token":         {Platform: "android"},
		"short token":      {Token: "abc", Platform: "android"},
		"token with space": {Token: "abcdefgh ijklmnop", Platform: "android"},
		"unknown platform": {Token: deviceToken, Platform: "windows"},
		"bad app":          {Token: deviceToken, Platform: "ios", App: "not an app!"},
		"bad locale":       {Token: deviceToken, Platform: "ios", Locale: "portuguese brazil"},
		"giant token":      {Token: strings.Repeat("a", 5000), Platform: "ios"},
	} {
		_, err := w.module.Devices().Register(context.Background(), ada, in)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if w.count(acme) != 0 {
		t.Fatal("an invalid registration wrote a row")
	}
}

func TestTheCleanRemovesStalePhonesOfEveryTenant(t *testing.T) {
	t.Parallel()
	w := install(t, false, func(c *fcm.Config) { c.StaleAfterDays = 30 })
	w.register(member(acme, "ada"), "tok-old-acme-0000000001", "android")
	w.register(member(globex, "ada"), "tok-old-globex-00000001", "ios")
	w.clock.advance(20 * 24 * time.Hour)
	w.register(member(acme, "bob"), "tok-new-acme-0000000001", "android")
	w.clock.advance(15 * 24 * time.Hour) // the first two are 35 days old, bob's 15

	tasks := w.module.Schedule()
	if len(tasks) != 1 || tasks[0].Action != fcm.DeviceClean || tasks[0].Spec != fcm.DefaultCleanSpec {
		t.Fatalf("schedule = %+v", tasks)
	}
	//arandu:system-grant the test plays the scheduler, which hands the task a grant of its own.
	if err := tasks[0].Run(context.Background(), security.SystemGrant(fcm.DeviceClean, "scheduler")); err != nil {
		t.Fatal(err)
	}

	if w.count(acme) != 1 || w.count(globex) != 0 {
		t.Fatalf("after the clean: acme %d (want bob's), globex %d (want none)", w.count(acme), w.count(globex))
	}
	if left := w.devices(acme, "bob"); len(left) != 1 {
		t.Fatalf("bob's recent phone was removed: %v", left)
	}
}

func TestTheCleanCanBeSwitchedOff(t *testing.T) {
	t.Parallel()
	w := install(t, false, func(c *fcm.Config) { c.CleanSpec = "-" })
	if tasks := w.module.Schedule(); len(tasks) != 0 {
		t.Fatalf("schedule = %+v", tasks)
	}
}

func TestEveryChangeIsInTheOutboxWithoutAToken(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	ada := member(acme, "ada")
	w.register(ada, deviceToken, "android")
	w.register(ada, deviceToken, "android")
	if err := w.module.Devices().Unregister(context.Background(), ada, deviceToken); err != nil {
		t.Fatal(err)
	}

	want := []string{fcm.EventDeviceRegistered, fcm.EventDeviceRegistered, fcm.EventDeviceUnregistered}
	if got := w.events(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	rows, err := w.db.QueryContext(context.Background(), "SELECT payload, tenant_id, authorized_by FROM outbox")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload, tenant, by string
		if err := rows.Scan(&payload, &tenant, &by); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(payload, deviceToken) || tenant != acme || by != "ada" {
			t.Errorf("outbox row: payload %s, tenant %s, by %s", payload, tenant, by)
		}
	}
}

func TestTheModuleDeclaresItsSchemaAndRollsItBack(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	declared := w.module.Migrations()
	if len(declared) != 1 {
		t.Fatalf("%d migrations declared", len(declared))
	}
	if declared[0].GetName() != "20261031_0001_create_fcm_devices" {
		t.Fatalf("migration name = %q", declared[0].GetName())
	}
}

func TestConcurrentRegistrationsOfOneTokenAreOneDevice(t *testing.T) {
	t.Parallel()
	w := install(t, false, nil)
	ada := member(acme, "ada")

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.module.Devices().Register(context.Background(), ada, fcm.RegisterRequest{Token: deviceToken, Platform: "android"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent registration failed: %v", err)
		}
	}
	if n := w.count(acme); n != 1 {
		t.Fatalf("%d rows after 8 concurrent registrations of one token, want 1", n)
	}
}
