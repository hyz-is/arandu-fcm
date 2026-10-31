package fcm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/arandu-io/hesape/auth"
	"github.com/arandu-io/hesape/notifications"
)

// Channel delivers a notification to the phones of its recipient. It is the
// hesape notifications.Channel for push: wire it into the Notifier beside mail
// and database, and a notification that returns ChannelFCM from Via and
// implements PushNotification reaches every phone its recipient has registered.
//
// Without a Queue in the Config the channel sends before it returns, one call
// per phone, and a failure is returned to the Notifier; there is no retry, and
// nothing is sent twice. With a Queue each phone gets a job, and the worker
// owns the retries.
type Channel struct {
	devices        *DeviceService
	pusher         *pusher
	notifiableType string
}

var _ notifications.Channel = (*Channel)(nil)

// Name is "fcm", the channel a notification names in Via.
func (*Channel) Name() notifications.ChannelName { return ChannelFCM }

// Send pushes n to the phones of to, and answers the receipt: the names Firebase
// gave the messages, or the ids of the jobs queued, joined by commas.
//
// The recipient's phones are those of the user in to.RouteFor(ChannelFCM), or
// else the NotifiableID of a recipient whose NotifiableType is the configured
// one ("user" by default). A recipient who is neither, or who has no phone, is
// notifications.ErrNotAddressed, which the Notifier treats as "not reachable
// this way" and not as a failure.
//
// A Grant that only reads is refused with auth.ErrReadOnly, as the Notifier
// refuses it, for a caller that reaches the channel without it. The tenant of
// every statement is the Grant's.
func (c *Channel) Send(ctx context.Context, g auth.Grant, to notifications.Notifiable, n notifications.Notification) (string, error) {
	if err := auth.Writable(g); err != nil {
		return "", err
	}
	buildable, ok := n.(PushNotification)
	if !ok {
		return "", fmt.Errorf("notifications: %s named the fcm channel and %T has no ToPush", n.Key(), n)
	}
	userID := c.recipient(to)
	if userID == "" {
		return "", notifications.ErrNotAddressed
	}
	devices, err := c.devices.devicesFor(ctx, g, userID)
	if err != nil {
		return "", err
	}
	if len(devices) == 0 {
		return "", notifications.ErrNotAddressed
	}

	pushes, err := c.pushesFor(buildable, to, devices)
	if err != nil {
		return "", fmt.Errorf("notifications: %s: %w", n.Key(), err)
	}

	var receipts []string
	var failures []error
	for i, device := range devices {
		var receipt string
		var err error
		if c.pusher.queue != nil {
			receipt, err = c.pusher.enqueue(ctx, g, device.ID, pushes[i])
		} else {
			receipt, err = c.pusher.deliver(ctx, g, device.ID, pushes[i])
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if receipt != "" {
			receipts = append(receipts, receipt)
		}
	}
	return strings.Join(receipts, ","), errors.Join(failures...)
}

// recipient is the user whose phones a notification reaches.
func (c *Channel) recipient(to notifications.Notifiable) string {
	if route := to.RouteFor(ChannelFCM); route != "" {
		return route
	}
	if to.NotifiableType() == c.notifiableType {
		return to.NotifiableID()
	}
	return ""
}

// pushesFor builds the push of each device and validates them all before
// anything is sent. A notification that words its push per language is asked
// once per language its recipient's phones use.
func (c *Channel) pushesFor(n PushNotification, to notifications.Notifiable, devices []*Device) ([]Push, error) {
	localized, isLocalized := n.(LocalizedPushNotification)
	byLocale := map[string]Push{}
	out := make([]Push, len(devices))
	for i, device := range devices {
		locale := device.Locale
		if !isLocalized {
			locale = ""
		}
		push, built := byLocale[locale]
		if !built {
			if isLocalized && locale != "" {
				push = localized.ToPushIn(to, locale)
			} else {
				push = n.ToPush(to)
			}
			if push.Locale == "" {
				push.Locale = locale
			}
			if err := push.Validate(); err != nil {
				return nil, err
			}
			byLocale[locale] = push
		}
		out[i] = push
	}
	return out, nil
}
