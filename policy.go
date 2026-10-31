package fcm

import (
	"context"
	"fmt"

	"github.com/arandu-io/framework/security"
)

// The actions of the package. Constants rather than strings at the call site: a
// typo in an action name would silently authorize nothing, or worse,
// everything. arandu-permission fills a Subject's Actions with them.
const (
	// DeviceRegister is registering the push token of the subject's own phone,
	// including taking over a token another user of the tenant had registered.
	DeviceRegister security.Action = "fcm.device.register"
	// DeviceUnregister is removing one of the subject's own phones.
	DeviceUnregister security.Action = "fcm.device.unregister"
	// DeviceList is reading the subject's own phones.
	DeviceList security.Action = "fcm.device.list"
	// DeviceManage is reading and removing the phones of any user of the
	// tenant, the access of whoever administers it.
	DeviceManage security.Action = "fcm.device.manage"

	// DeviceClean is removing the phones that have not registered themselves
	// for longer than the configured time. Nobody holds it: the scheduled clean
	// issues its own Grant for each tenant that has devices, and the policy
	// refuses it to any subject.
	DeviceClean security.Action = "fcm.device.clean"
)

// Actions are the actions a person can be given, for arandu-permission to
// offer. Sending is not among them: a push is sent by the notification the
// application dispatches, under the Grant that authorized the notification, and
// the notifications package asks its own action for it.
func Actions() []security.Action {
	return []security.Action{DeviceRegister, DeviceUnregister, DeviceList, DeviceManage}
}

// DevicePolicy is the only authority over who does what with a Device.
//
// It denies unless the subject of the device's tenant carries the action, and a
// person's own phones are theirs alone: reading or removing another user's
// phone takes DeviceManage.
type DevicePolicy struct{}

// Compile-time proof that the policy answers about this entity and no other.
var _ security.Policy[Device] = DevicePolicy{}

// Writes says which actions change data, so the Grant of a read action cannot
// be used to write.
func (DevicePolicy) Writes(a security.Action) bool { return a != DeviceList }

// Can decides whether the subject may perform the action on the device.
func (DevicePolicy) Can(_ context.Context, s security.Subject, a security.Action, record Device) error {
	// Tenant isolation comes first and applies to every action. The empty id is
	// the candidate that has not been stored yet, or the probe a listing asks.
	if record.ID != "" && record.TenantID != s.Tenant {
		return fmt.Errorf("device belongs to another tenant")
	}

	// arandu:begin custom
	if s.IsGuest() {
		return fmt.Errorf("a guest has no device")
	}
	switch a {
	case DeviceRegister:
		// A token seen again replaces its earlier registration whoever held it:
		// presenting the token is the proof of the phone. The device that
		// results is the subject's, never anybody else's.
		if s.Can(DeviceRegister) {
			return nil
		}
	case DeviceUnregister, DeviceList:
		if s.Can(DeviceManage) {
			return nil
		}
		if s.Can(a) && record.UserID == s.ID {
			return nil
		}
	}
	// arandu:end custom

	return fmt.Errorf("no rule allows %s on device", a)
}
