package fcm

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arandu-io/hesape/notifications"
)

// ChannelFCM is the channel name a notification returns from Via to reach the
// phones of its recipient. It is the module's own: hesape ships mail, database
// and broadcast, and a ChannelName is a string precisely so a project can add
// one.
const ChannelFCM notifications.ChannelName = "fcm"

// Priority is how urgently Firebase and the platform deliver a push.
type Priority string

const (
	// PriorityHigh wakes the device and shows the message now. It is what an
	// empty priority means: a push a person is meant to see.
	PriorityHigh Priority = "high"
	// PriorityNormal lets the platform batch the delivery to save battery.
	PriorityNormal Priority = "normal"
)

// The limits a push is held to before anything is sent. They are Firebase's:
// a message over them is refused by the service with an error that does not say
// which part was too much.
const (
	// MaxPayloadBytes is the size Firebase accepts for the notification and the
	// data together.
	MaxPayloadBytes = 4096
	// MaxTTL is the longest Firebase keeps a message for a device that is
	// offline: four weeks.
	MaxTTL = 28 * 24 * time.Hour
	// MaxDataKeys bounds the key-value pairs of one push.
	MaxDataKeys = 50
	// MaxCollapseKeyBytes is the length of the key Firebase accepts to replace
	// an earlier push with a later one.
	MaxCollapseKeyBytes = 64
	// MaxTitleRunes and MaxBodyRunes keep the visible text to what the
	// platforms show before they cut it.
	MaxTitleRunes = 200
	MaxBodyRunes  = 1000
)

// Push is the message a notification sends to a phone, as messages.Mail is the
// message it sends by e-mail: structured fields and chaining helpers, checked
// by Validate before a transport is touched.
//
// The fields and the methods of a Go type share one namespace, so the setters
// keep the short names and the fields say what they hold, as in messages.Mail.
// The JSON names are the module's job payload and its stable wire form.
type Push struct {
	// TitleText and BodyText are what the notification shows. Both empty makes
	// a data-only push, delivered to the application without a visible alert.
	TitleText string `json:"title,omitempty"`
	BodyText  string `json:"body,omitempty"`
	// ImageURL is an https image shown with the alert.
	ImageURL string `json:"image,omitempty"`
	// DataValues are the key-value pairs the application receives with the
	// push, the usual carrier of a deep link. Firebase delivers only strings.
	DataValues map[string]string `json:"data,omitempty"`
	// PriorityLevel is the urgency. Empty means PriorityHigh.
	PriorityLevel Priority `json:"priority,omitempty"`
	// CollapseKeyName makes a later push with the same key replace an earlier
	// one that was not delivered yet.
	CollapseKeyName string `json:"collapse_key,omitempty"`
	// TimeToLive is how long Firebase keeps the push for an offline device.
	// Zero leaves Firebase's own default, four weeks.
	TimeToLive time.Duration `json:"ttl,omitempty"`
	// ClickActionName is the Android intent action a tap on the notification
	// opens. It has no iOS counterpart: an iOS tap opens the application and
	// hands it the data.
	ClickActionName string `json:"click_action,omitempty"`
	// Locale is the language the push was built for, set by the channel from
	// the device.
	Locale string `json:"locale,omitempty"`
}

// PushNotification is what a notification implements to travel by push, as
// channels.MailNotification is what it implements to travel by e-mail.
type PushNotification interface {
	// ToPush is the message, built for this recipient.
	ToPush(to notifications.Notifiable) Push
}

// LocalizedPushNotification is the optional half of PushNotification: a
// notification that words its push differently for each language its recipient's
// phones are set to. The channel asks it once per distinct device language and
// falls back to ToPush for devices that did not say.
type LocalizedPushNotification interface {
	PushNotification
	// ToPushIn is the message in this language, a BCP 47 tag.
	ToPushIn(to notifications.Notifiable, locale string) Push
}

// NewPush returns an empty push, ready for the chaining helpers.
func NewPush() Push { return Push{} }

// Title sets the title of the alert.
func (p Push) Title(title string) Push {
	p.TitleText = title
	return p
}

// Body sets the text of the alert.
func (p Push) Body(body string) Push {
	p.BodyText = body
	return p
}

// Image sets the https image shown with the alert.
func (p Push) Image(url string) Push {
	p.ImageURL = url
	return p
}

// With adds one key-value pair the application receives, such as the deep link
// of the screen the push is about.
func (p Push) With(key, value string) Push {
	next := make(map[string]string, len(p.DataValues)+1)
	for k, v := range p.DataValues {
		next[k] = v
	}
	next[key] = value
	p.DataValues = next
	return p
}

// Data replaces the key-value pairs the application receives.
func (p Push) Data(data map[string]string) Push {
	p.DataValues = make(map[string]string, len(data))
	for k, v := range data {
		p.DataValues[k] = v
	}
	return p
}

// Priority sets the urgency.
func (p Push) Priority(level Priority) Push {
	p.PriorityLevel = level
	return p
}

// Normal lowers the urgency to PriorityNormal.
func (p Push) Normal() Push { return p.Priority(PriorityNormal) }

// CollapseKey sets the key a later push replaces this one by.
func (p Push) CollapseKey(key string) Push {
	p.CollapseKeyName = key
	return p
}

// TTL sets how long Firebase keeps the push for an offline device.
func (p Push) TTL(ttl time.Duration) Push {
	p.TimeToLive = ttl
	return p
}

// ClickAction sets the Android intent action a tap opens.
func (p Push) ClickAction(action string) Push {
	p.ClickActionName = action
	return p
}

// Urgency is the priority the push goes with: PriorityHigh unless it asked for
// PriorityNormal.
func (p Push) Urgency() Priority {
	if p.PriorityLevel == PriorityNormal {
		return PriorityNormal
	}
	return PriorityHigh
}

// Silent reports whether the push has no alert and carries only data.
func (p Push) Silent() bool {
	return strings.TrimSpace(p.TitleText) == "" && strings.TrimSpace(p.BodyText) == ""
}

// ToArray is the push as the keys of its wire form, for a log line or a test.
func (p Push) ToArray() map[string]any {
	return map[string]any{
		"title": p.TitleText, "body": p.BodyText, "image": p.ImageURL,
		"data": p.DataValues, "priority": string(p.Urgency()),
		"collapse_key": p.CollapseKeyName, "ttl": int64(p.TimeToLive / time.Second),
		"click_action": p.ClickActionName,
	}
}

// reservedDataKeys are the names Firebase keeps for itself in a data message.
var reservedDataKeys = map[string]bool{
	"from": true, "notification": true, "message_type": true,
}

// Validate reports what would make Firebase refuse the push, or make it
// useless to receive. The channel calls it before a transport is touched.
func (p Push) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if p.Silent() && len(p.DataValues) == 0 {
		add("it has no title, no body and no data")
	}
	if utf8.RuneCountInString(p.TitleText) > MaxTitleRunes {
		add("the title is longer than %d characters", MaxTitleRunes)
	}
	if utf8.RuneCountInString(p.BodyText) > MaxBodyRunes {
		add("the body is longer than %d characters", MaxBodyRunes)
	}
	if p.ImageURL != "" {
		if u, err := url.Parse(p.ImageURL); err != nil || u.Scheme != "https" || u.Host == "" {
			add("the image is not an absolute https address")
		}
	}
	if p.PriorityLevel != "" && p.PriorityLevel != PriorityHigh && p.PriorityLevel != PriorityNormal {
		add("the priority is neither high nor normal")
	}
	if len(p.CollapseKeyName) > MaxCollapseKeyBytes {
		add("the collapse key is longer than %d bytes", MaxCollapseKeyBytes)
	}
	if p.TimeToLive < 0 || p.TimeToLive > MaxTTL {
		add("the ttl is outside 0 to %s", MaxTTL)
	}
	if len(p.ClickActionName) > 200 {
		add("the click action is longer than 200 bytes")
	}
	if len(p.DataValues) > MaxDataKeys {
		add("it carries %d data pairs and the limit is %d", len(p.DataValues), MaxDataKeys)
	}
	keys := make([]string, 0, len(p.DataValues))
	for k := range p.DataValues {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	size := len(p.TitleText) + len(p.BodyText)
	for _, k := range keys {
		lower := strings.ToLower(k)
		switch {
		case k == "":
			add("a data key is empty")
		case reservedDataKeys[lower] || strings.HasPrefix(lower, "google") || strings.HasPrefix(lower, "gcm"):
			add("the data key %q is reserved by Firebase", k)
		case !validDataKey(k):
			add("the data key %q has characters Firebase refuses", k)
		}
		size += len(k) + len(p.DataValues[k])
	}
	if size > MaxPayloadBytes {
		add("the title, body and data are %d bytes and the limit is %d", size, MaxPayloadBytes)
	}
	if len(problems) > 0 {
		return errors.New("fcm: the push is not valid: " + strings.Join(problems, "; "))
	}
	return nil
}

// validDataKey is the character set Firebase accepts for a data key.
func validDataKey(key string) bool {
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}
