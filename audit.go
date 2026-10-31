package fcm

import (
	"context"
	"time"

	"github.com/arandu-io/framework/data"
	"github.com/arandu-io/framework/security"
	"github.com/arandu-io/hesape/events"
)

// The events every change to the devices stores in the application's outbox, in
// the same transaction as the change. The outbox row carries the tenant, the
// subject that was authorized and the action, so an events.Publisher such as
// arandu-activitylog's EventRecorder writes who did what without this module
// depending on it. A payload never carries a token, sealed or not.
const (
	// EventDeviceRegistered is a phone registered, or registered again; the
	// payload says whether it changed hands.
	EventDeviceRegistered = "fcm.device.registered"
	// EventDeviceUnregistered is a phone removed by the person who holds it or
	// by whoever manages the tenant.
	EventDeviceUnregistered = "fcm.device.unregistered"
	// EventDeviceRemoved is a phone removed by the module itself, with the
	// reason: Firebase said the token is dead, the phone went stale, or its
	// owner passed the limit.
	EventDeviceRemoved = "fcm.device.removed"
)

// The aggregate the events are about.
const aggregateDevice = "fcm_device"

// The reasons a device is removed by the module.
const (
	// RemovedUnregistered is Firebase answering UNREGISTERED.
	RemovedUnregistered = "unregistered"
	// RemovedInvalidToken is Firebase answering INVALID_ARGUMENT about the
	// token.
	RemovedInvalidToken = "invalid_token"
	// RemovedStale is the scheduled clean finding no registration for longer
	// than the configured time.
	RemovedStale = "stale"
	// RemovedLimit is a user passing the number of phones the module keeps for
	// one person; the least recently seen goes.
	RemovedLimit = "limit"
)

// auditor stores the events in the outbox on the module's database handle.
type auditor struct {
	outbox *events.Outbox
	now    func() time.Time
}

func newAuditor(db *data.DB, now func() time.Time) *auditor {
	return &auditor{outbox: events.NewOutbox(outboxHandle{db}), now: now}
}

// record stores one event. It runs inside the transaction of the change it
// records, and fails it when it cannot be stored: a change the log does not
// know of is a change that did not happen.
func (a *auditor) record(ctx context.Context, g security.Grant, name, id string, payload map[string]any) error {
	return a.outbox.Store(ctx, g, []events.Event{{
		Name: name, Aggregate: aggregateDevice, AggregateID: id, Payload: payload, OccurredAt: a.now().UTC(),
	}})
}

// outboxHandle is the module's handle as the outbox reads it.
type outboxHandle struct{ *data.DB }

// InTransaction reports whether ctx is inside a transaction on the handle.
func (h outboxHandle) InTransaction(ctx context.Context) bool { return data.InTransaction(ctx, h.DB) }

// deviceFacts is what the log keeps of a device: nothing sealed.
func deviceFacts(d *Device) map[string]any {
	return map[string]any{"user_id": d.UserID, "platform": string(d.Platform), "app": d.App, "locale": d.Locale}
}
