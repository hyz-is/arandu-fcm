package fcm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/arandu-io/hesape/auth"
	"github.com/arandu-io/hesape/notifications"
	"github.com/arandu-io/hesape/queue"
	"github.com/arandu-io/hesape/queue/attributes"
	"github.com/arandu-io/hesape/queue/jobs"
)

const (
	// DefaultQueue is the queue the pushes are put on when a Queue is given and
	// Config.QueueName is empty. Run its worker with `aru queue:work --queue=fcm`.
	DefaultQueue = "fcm"
	// JobPush is the stable name of the job that delivers one push to one
	// device.
	JobPush = "fcm.push"
	// DefaultTries is how many deliveries a push job gets before it is parked.
	DefaultTries = 5
	// defaultRetryWindow is how long a queued push without a ttl stays worth
	// delivering. A notification that could not be delivered in an hour is not
	// news.
	defaultRetryWindow = time.Hour
)

// defaultBackoff is the wait before each retry of a push; the last repeats. A
// Retry-After from Firebase holds the retry at least that long.
var defaultBackoff = []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// pushJob is the payload of the job that delivers one push to one device. It
// names the device and never carries its token: the worker reads and opens the
// token when it sends.
//
// ExpiresAt is when the push stops being worth delivering: its ttl, or an hour.
// The worker's tries bound how often it is attempted and this bounds how late,
// so a retry that comes after the ttl is dropped instead of sent. The queue's
// own retry deadline is not used because it replaces the tries, and the tries
// are what keep a push from being sent in a loop.
type pushJob struct {
	DeviceID  string    `json:"device_id"`
	Push      Push      `json:"push"`
	ExpiresAt time.Time `json:"expires_at"`
}

// pusher delivers pushes to devices, directly or through the queue.
type pusher struct {
	devices   *DeviceService
	sender    Sender
	queue     queue.Queue
	queueName string
	tries     int
	now       func() time.Time
	log       *slog.Logger
}

// deliver sends one push to one device.
//
// A phone Firebase says is gone -- UNREGISTERED, or INVALID_ARGUMENT pointing at
// the token -- is removed and is not an error: its owner's other phones are
// still reachable, and the same push would fail the same way. Any other failure
// is returned as a *SendError for the caller to retry or give up on.
func (p *pusher) deliver(ctx context.Context, g auth.Grant, deviceID string, push Push) (string, error) {
	device, err := p.devices.deviceByID(ctx, g, deviceID)
	if err != nil {
		return "", err
	}
	if device == nil {
		// Removed since the push was queued: nothing to deliver to.
		return "", nil
	}
	token, err := p.devices.open(device)
	if err != nil {
		return "", err
	}
	name, err := p.sender.Send(ctx, Outgoing{Token: token, Platform: device.Platform, Push: push})
	if err == nil {
		return name, nil
	}

	var failure *SendError
	if errors.As(err, &failure) && failure.InvalidToken {
		reason := RemovedInvalidToken
		if failure.Code == "UNREGISTERED" {
			reason = RemovedUnregistered
		}
		if err := p.devices.removeDead(ctx, g, device, reason); err != nil {
			return "", err
		}
		p.log.InfoContext(ctx, "fcm: device removed", "device", device, "reason", reason)
		return "", nil
	}
	if failure != nil {
		p.log.WarnContext(ctx, "fcm: push not delivered", "device", device,
			"status", failure.Status, "code", failure.Code, "retryable", failure.Retryable(),
			"retry_after", failure.RetryAfter)
	}
	return "", err
}

// enqueue puts the delivery of one push on the queue, under the Grant the
// notification was authorized with, and answers the job's id.
func (p *pusher) enqueue(ctx context.Context, g auth.Grant, deviceID string, push Push) (string, error) {
	window := defaultRetryWindow
	if push.TimeToLive > 0 {
		window = push.TimeToLive
	}
	job, err := jobs.New(g, p.queueName, JobPush, pushJob{
		DeviceID: deviceID, Push: push, ExpiresAt: p.now().UTC().Add(window),
	})
	if err != nil {
		return "", err
	}
	job.Attributes = attributes.Attributes{
		Queue:   p.queueName,
		Tries:   p.tries,
		Backoff: defaultBackoff,
	}
	if err := p.queue.Push(ctx, g, job); err != nil {
		return "", err
	}
	return job.UUID, nil
}

// handle is the job: it delivers one push and settles the outcome with the
// worker. A failure that can pass -- throttling, an unavailable service, a call
// that did not complete -- is returned as it is, and the worker retries it after
// the backoff, or after the Retry-After Firebase asked for when that is longer,
// until the job's tries or its deadline run out. A failure that cannot pass is
// returned as a manual failure and the job is parked once, never retried.
func (p *pusher) handle(ctx context.Context, g auth.Grant, _ *jobs.Job, in pushJob) error {
	if err := g.Check(notifications.ActionSend); err != nil {
		return fmt.Errorf("%w: %w", queue.ErrManuallyFailed, err)
	}
	if !in.ExpiresAt.IsZero() && !p.now().Before(in.ExpiresAt) {
		// Too late to be news, and for a push with a ttl Firebase would drop it
		// at once anyway.
		p.log.InfoContext(ctx, "fcm: queued push expired before it was delivered", "device_id", in.DeviceID)
		return nil
	}
	_, err := p.deliver(ctx, g, in.DeviceID, in.Push)
	if err == nil {
		return nil
	}
	var failure *SendError
	if errors.As(err, &failure) {
		if failure.Retryable() {
			return err
		}
		return fmt.Errorf("%w: %w", queue.ErrManuallyFailed, err)
	}
	if errors.Is(err, errUnsealable) || errors.Is(err, errSealedElsewhere) || errors.Is(err, auth.ErrForbidden) {
		return fmt.Errorf("%w: %w", queue.ErrManuallyFailed, err)
	}
	// Anything else is the database or the context: it may pass.
	return err
}
