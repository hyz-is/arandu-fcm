package fcm

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is the only place the module speaks HTTP: the Firebase Cloud
// Messaging v1 call that delivers one message to one device, and the OAuth2
// exchange that authorizes it. Everything else in the package reaches Firebase
// through the Sender interface, which is what the tests and an application's
// own tests replace.

// DefaultAPIBase is where Firebase Cloud Messaging answers.
const DefaultAPIBase = "https://fcm.googleapis.com"

// Scope is the OAuth2 scope the access token is issued for.
const Scope = "https://www.googleapis.com/auth/firebase.messaging"

const (
	// maxResponseBytes bounds what is read from either endpoint.
	maxResponseBytes = 1 << 20
	// defaultHTTPTimeout bounds one call when the application passes no client.
	defaultHTTPTimeout = 20 * time.Second
	// tokenLifetime is how long the signed assertion asks the access token to
	// live: the maximum Google accepts.
	tokenLifetime = time.Hour
	// refreshMargin is how long before it expires a cached access token is
	// replaced, so a send never starts with a token about to lapse.
	refreshMargin = 5 * time.Minute
	// maxErrorText bounds the provider text kept in an error.
	maxErrorText = 300
	// maxRetryAfter bounds a Retry-After the provider asks for.
	maxRetryAfter = 24 * time.Hour
)

// Outgoing is one message for one device, as the Sender receives it: the
// registration token in clear, the platform that decides the shape of the
// message, and the push. It prints without the token.
type Outgoing struct {
	Token    string
	Platform Platform
	Push     Push
}

// String describes the message without the token.
func (o Outgoing) String() string {
	return fmt.Sprintf("fcm.Outgoing{platform:%s token:%s}", o.Platform, redacted)
}

// GoString is what %#v prints.
func (o Outgoing) GoString() string { return o.String() }

// Format answers every fmt verb with String, so no verb reaches the token.
func (o Outgoing) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(o.String())) }

// LogValue is what slog records.
func (o Outgoing) LogValue() slog.Value {
	return slog.GroupValue(slog.String("platform", string(o.Platform)), slog.String("token", redacted))
}

// MarshalJSON encodes the message without the token.
func (o Outgoing) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"platform": o.Platform, "token": redacted, "push": o.Push})
}

// Sender delivers one message to one device and answers the name Firebase gave
// it. It is the seam between the module and the network: [Client] is the one
// implementation that reaches Firebase and [Fake] is the one an application's
// tests use.
//
// A failure is a *SendError, which says whether the token is dead, whether to
// try again and when.
type Sender interface {
	Send(ctx context.Context, out Outgoing) (name string, err error)
}

// SendError is a delivery that Firebase, or the exchange that authorizes it,
// refused or could not complete. It never carries a credential, a token or a
// message body.
type SendError struct {
	// Status is the HTTP status, zero when the call did not complete.
	Status int
	// Code is Firebase's error code (UNREGISTERED, INVALID_ARGUMENT,
	// QUOTA_EXCEEDED, UNAVAILABLE...) or the OAuth error (invalid_grant), empty
	// when the call did not complete.
	Code string
	// Message is the provider's text with every secret taken out and cut short.
	Message string
	// RetryAfter is how long the provider asked to wait, zero when it did not.
	RetryAfter time.Duration
	// InvalidToken says the failure is about the registration token itself: it
	// is not registered any more, or is not a token at all. The device it was
	// stored for is dead.
	InvalidToken bool

	cause error
}

// Error describes the failure without any secret.
func (e *SendError) Error() string {
	var b strings.Builder
	b.WriteString("fcm: ")
	switch {
	case e.Status == 0:
		b.WriteString("the call to Firebase did not complete")
	default:
		fmt.Fprintf(&b, "Firebase answered %d", e.Status)
	}
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// Unwrap is the transport error behind a call that did not complete.
func (e *SendError) Unwrap() error { return e.cause }

// Retryable reports whether sending the same message again later can succeed:
// the provider is throttling or unavailable, or the call did not complete. A
// refused message or a dead token is not retryable, and sending it again would
// only repeat the refusal.
func (e *SendError) Retryable() bool {
	if e.InvalidToken {
		return false
	}
	switch {
	case e.Status == 0, e.Status == http.StatusTooManyRequests:
		return true
	case e.Status >= 500 && e.Status <= 599:
		return true
	}
	return false
}

// RetryAfterDuration is how long the provider asked to wait. The queue worker
// reads it, structurally, to hold the retry at least that long.
func (e *SendError) RetryAfterDuration() time.Duration { return e.RetryAfter }

// ClientConfig is what [NewClient] needs.
type ClientConfig struct {
	// Credentials is the service account the client signs in with.
	Credentials Credentials
	// APIBase is where messages are sent; empty means DefaultAPIBase. It is
	// https unless the host is loopback.
	APIBase string
	// HTTPClient carries both calls; nil means a client with a timeout that
	// follows no redirect. A client passed in keeps its transport.
	HTTPClient *http.Client
	// Clock is the time the client reads; nil means time.Now.
	Clock func() time.Time
}

// Client sends messages through the Firebase Cloud Messaging HTTP v1 API.
//
// It signs in with the service account: a JWT signed RS256 is traded for an
// access token at the token address of the account, and the token is kept until
// shortly before it expires. A 401 from the send replaces the cached token and
// repeats the send once.
type Client struct {
	api      string
	project  string
	http     *http.Client
	now      func() time.Time
	tokens   *tokenSource
	scrubber *scrubber
}

var _ Sender = (*Client)(nil)

// NewClient returns the client, or the reason it cannot be built.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Credentials.IsZero() {
		return nil, errors.New("fcm: NewClient needs the service account credentials")
	}
	api := strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/")
	if api == "" {
		api = DefaultAPIBase
	}
	if err := checkEndpoint("APIBase", api); err != nil {
		return nil, err
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	httpClient := isolatedHTTPClient(cfg.HTTPClient)
	account := cfg.Credentials.account

	sc := newScrubber()
	// The key as a provider could echo it: whole, or as the 64 character lines
	// of its PEM block, in either of the encodings a service account uses.
	if der, err := x509.MarshalPKCS8PrivateKey(account.key); err == nil {
		sc.addPEMBody(base64.StdEncoding.EncodeToString(der))
	}
	sc.addPEMBody(base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(account.key)))
	return &Client{
		api:      api,
		project:  account.projectID,
		http:     httpClient,
		now:      now,
		tokens:   &tokenSource{account: account, http: httpClient, now: now, scrub: sc},
		scrubber: sc,
	}, nil
}

// isolatedHTTPClient is the client both calls go through: the application's, or
// one with a timeout, and never one that follows a redirect, which would carry
// the access token to wherever it pointed.
func isolatedHTTPClient(given *http.Client) *http.Client {
	client := &http.Client{}
	if given != nil {
		copied := *given
		client = &copied
	}
	if client.Timeout == 0 {
		client.Timeout = defaultHTTPTimeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

// Send delivers one message and answers the name Firebase gave it.
func (c *Client) Send(ctx context.Context, out Outgoing) (string, error) {
	body, err := buildBody(out, c.now())
	if err != nil {
		return "", err
	}
	c.scrubber.add(out.Token)
	endpoint := c.api + "/v1/projects/" + url.PathEscape(c.project) + "/messages:send"

	for attempt := 0; ; attempt++ {
		access, err := c.tokens.get(ctx)
		if err != nil {
			return "", err
		}
		status, header, raw, err := c.post(ctx, endpoint, access, body)
		if err != nil {
			return "", &SendError{Message: c.scrubber.clean(transportText(err)), cause: contextCause(err)}
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.tokens.invalidate(access)
			continue
		}
		return c.interpret(status, header, raw)
	}
}

// post makes one call with the access token.
func (c *Client) post(ctx context.Context, endpoint, access string, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// contextCause keeps the cancellation or the deadline of the caller's context
// as the cause of a failed call, and nothing else of the transport error: that
// error repeats the address and is not worth carrying into a log line.
func contextCause(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	}
	return nil
}

// transportText is what a failed call says, without the address of the call: a
// URL error repeats it, and it is nobody's business in a log line.
func transportText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return err.Error()
}

// fcmError is the error body of the v1 API.
type fcmError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			Type       string `json:"@type"`
			ErrorCode  string `json:"errorCode"`
			Violations []struct {
				Field string `json:"field"`
			} `json:"fieldViolations"`
		} `json:"details"`
	} `json:"error"`
}

// interpret turns the answer of the send into a name or a SendError.
func (c *Client) interpret(status int, header http.Header, raw []byte) (string, error) {
	if status >= 200 && status < 300 {
		var ok struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &ok); err != nil || ok.Name == "" {
			return "", &SendError{Status: status, Message: "the answer carries no message name"}
		}
		return ok.Name, nil
	}

	failure := &SendError{Status: status, RetryAfter: parseRetryAfter(header.Get("Retry-After"), c.now())}
	var body fcmError
	if json.Unmarshal(raw, &body) == nil && (body.Error.Status != "" || body.Error.Message != "") {
		failure.Code = body.Error.Status
		failure.Message = c.scrubber.clean(body.Error.Message)
		tokenFault := false
		for _, detail := range body.Error.Details {
			if detail.ErrorCode != "" {
				failure.Code = detail.ErrorCode
			}
			for _, violation := range detail.Violations {
				if violation.Field == "message.token" {
					tokenFault = true
				}
			}
		}
		// UNREGISTERED is the token being gone. INVALID_ARGUMENT is the token
		// only when Firebase points at it: the same code also answers a payload
		// it will not take, and that must not cost the user a device.
		failure.InvalidToken = failure.Code == "UNREGISTERED" ||
			(failure.Code == "INVALID_ARGUMENT" && tokenFault)
	}
	return "", failure
}

// parseRetryAfter reads a Retry-After header: seconds, or an HTTP date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var wait time.Duration
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		wait = at.Sub(now)
	}
	return min(max(wait, 0), maxRetryAfter)
}

// scrubber takes secrets out of any text that is about to become an error or a
// log line: the registration tokens being sent, the assertion and the access
// token of the exchange, the key itself.
//
// The key is remembered for good. What passes through is remembered up to a
// bound and then forgotten, oldest and newest alike, so a process that sends to
// a million phones does not keep a million tokens in memory for it.
type scrubber struct {
	mu      sync.Mutex
	fixed   map[string]struct{}
	passing map[string]struct{}
}

func newScrubber() *scrubber {
	return &scrubber{fixed: map[string]struct{}{}, passing: map[string]struct{}{}}
}

// minScrubbed is the shortest value remembered: replacing a three letter string
// everywhere would mangle the text it was meant to protect.
const minScrubbed = 8

// maxPassing bounds the values that passed through and are remembered.
const maxPassing = 4096

// add remembers a value that passed through, such as a token or an assertion.
func (s *scrubber) add(secret string) {
	if len(secret) < minScrubbed {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.passing) >= maxPassing {
		clear(s.passing)
	}
	s.passing[secret] = struct{}{}
}

// addPEMBody remembers a key body for good, whole and line by line.
func (s *scrubber) addPEMBody(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(body) >= minScrubbed {
		s.fixed[body] = struct{}{}
	}
	for len(body) > 0 {
		line := body[:min(len(body), 64)]
		if len(line) >= minScrubbed {
			s.fixed[line] = struct{}{}
		}
		body = body[len(line):]
	}
}

// clean replaces every remembered secret and cuts the text short.
func (s *scrubber) clean(text string) string {
	s.mu.Lock()
	for _, set := range []map[string]struct{}{s.fixed, s.passing} {
		for secret := range set {
			text = strings.ReplaceAll(text, secret, redacted)
		}
	}
	s.mu.Unlock()
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxErrorText {
		text = text[:maxErrorText] + "..."
	}
	return text
}

// tokenSource trades the service account for access tokens and keeps the
// current one until shortly before it expires.
type tokenSource struct {
	account *serviceAccount
	http    *http.Client
	now     func() time.Time
	scrub   *scrubber

	mu      sync.Mutex
	token   string
	expires time.Time
}

// get answers a valid access token, exchanging a new assertion when the cached
// one is missing or about to lapse.
func (t *tokenSource) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Add(refreshMargin).Before(t.expires) {
		return t.token, nil
	}
	token, expires, err := t.exchange(ctx)
	if err != nil {
		return "", err
	}
	t.token, t.expires = token, expires
	return token, nil
}

// invalidate drops the cached token if it is still the one that was refused.
func (t *tokenSource) invalidate(stale string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == stale {
		t.token, t.expires = "", time.Time{}
	}
}

// exchange signs an assertion and trades it for an access token.
func (t *tokenSource) exchange(ctx context.Context) (string, time.Time, error) {
	issued := t.now()
	assertion, err := signAssertion(t.account, issued)
	if err != nil {
		return "", time.Time{}, &SendError{Message: "the assertion could not be signed"}
	}
	t.scrub.add(assertion)

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.account.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, &SendError{Message: "the token request could not be built"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return "", time.Time{}, &SendError{Code: "token_exchange", Message: t.scrub.clean(transportText(err)), cause: contextCause(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", time.Time{}, &SendError{Code: "token_exchange", Message: "the token answer could not be read", cause: contextCause(err)}
	}

	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &answer)
	if answer.AccessToken != "" {
		t.scrub.add(answer.AccessToken)
	}
	if resp.StatusCode != http.StatusOK || answer.AccessToken == "" {
		code := answer.Error
		if code == "" {
			code = "token_exchange"
		}
		status := resp.StatusCode
		if status == http.StatusOK {
			status = http.StatusBadGateway
		}
		return "", time.Time{}, &SendError{
			Status:     status,
			Code:       t.scrub.clean(code),
			Message:    t.scrub.clean(answer.Description),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), issued),
		}
	}
	lifetime := time.Duration(answer.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = tokenLifetime
	}
	return answer.AccessToken, issued.Add(lifetime), nil
}

// signAssertion builds the JWT the service account trades for an access token,
// signed RS256.
func signAssertion(account *serviceAccount, issued time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if account.privateKeyID != "" {
		header["kid"] = account.privateKeyID
	}
	claims := map[string]any{
		"iss":   account.clientEmail,
		"scope": Scope,
		"aud":   account.tokenURI,
		"iat":   issued.Unix(),
		"exp":   issued.Add(tokenLifetime).Unix(),
	}
	encode := func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(raw), nil
	}
	h, err := encode(header)
	if err != nil {
		return "", err
	}
	c, err := encode(claims)
	if err != nil {
		return "", err
	}
	signing := h + "." + c
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, account.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// parseURL reads an absolute http or https address.
func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("not an absolute http or https address")
	}
	return u, nil
}

// isLoopback reports whether a host is the machine itself.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// The wire form of the v1 message. Structs rather than maps, so the body is the
// same bytes for the same push.
type (
	wireEnvelope struct {
		Message wireMessage `json:"message"`
	}
	wireMessage struct {
		Token        string            `json:"token"`
		Notification *wireNotification `json:"notification,omitempty"`
		Data         map[string]string `json:"data,omitempty"`
		Android      *wireAndroid      `json:"android,omitempty"`
		APNS         *wireAPNS         `json:"apns,omitempty"`
	}
	wireNotification struct {
		Title string `json:"title,omitempty"`
		Body  string `json:"body,omitempty"`
	}
	wireAndroid struct {
		CollapseKey  string                   `json:"collapse_key,omitempty"`
		Priority     string                   `json:"priority"`
		TTL          string                   `json:"ttl,omitempty"`
		Notification *wireAndroidNotification `json:"notification,omitempty"`
	}
	wireAndroidNotification struct {
		Image       string `json:"image,omitempty"`
		ClickAction string `json:"click_action,omitempty"`
	}
	wireAPNS struct {
		Headers    map[string]string `json:"headers"`
		Payload    wireAPNSPayload   `json:"payload"`
		FCMOptions *wireFCMOptions   `json:"fcm_options,omitempty"`
	}
	wireAPNSPayload struct {
		APS wireAPS `json:"aps"`
	}
	wireAPS struct {
		Sound            string `json:"sound,omitempty"`
		MutableContent   int    `json:"mutable-content,omitempty"`
		ContentAvailable int    `json:"content-available,omitempty"`
	}
	wireFCMOptions struct {
		Image string `json:"image,omitempty"`
	}
)

// buildBody maps a push to the v1 message for the platform of the device.
//
// An Android device gets the android block: priority, collapse key, ttl, the
// image and the click action. An iPhone gets the apns block: apns-priority 10
// for a push that must be shown now (5 for a normal one), the default sound,
// mutable-content when there is an image for the notification service
// extension to download, apns-expiration from the ttl and apns-collapse-id from
// the collapse key. A push with no alert is a data message: content-available
// on iOS, with the priority 5 and the background push type APNs requires of it.
func buildBody(out Outgoing, now time.Time) ([]byte, error) {
	if strings.TrimSpace(out.Token) == "" {
		return nil, errors.New("fcm: the message has no registration token")
	}
	if out.Platform != PlatformAndroid && out.Platform != PlatformIOS {
		return nil, fmt.Errorf("fcm: the platform %q is neither android nor ios", out.Platform)
	}
	if err := out.Push.Validate(); err != nil {
		return nil, err
	}
	p := out.Push
	message := wireMessage{Token: out.Token, Data: p.DataValues}
	if !p.Silent() {
		message.Notification = &wireNotification{Title: p.TitleText, Body: p.BodyText}
	}

	switch out.Platform {
	case PlatformAndroid:
		android := &wireAndroid{CollapseKey: p.CollapseKeyName, Priority: "HIGH"}
		if p.Urgency() == PriorityNormal {
			android.Priority = "NORMAL"
		}
		if p.TimeToLive > 0 {
			android.TTL = strconv.FormatInt(ceilSeconds(p.TimeToLive), 10) + "s"
		}
		if !p.Silent() && (p.ImageURL != "" || p.ClickActionName != "") {
			android.Notification = &wireAndroidNotification{Image: p.ImageURL, ClickAction: p.ClickActionName}
		}
		message.Android = android
	case PlatformIOS:
		apns := &wireAPNS{Headers: map[string]string{}}
		switch {
		case p.Silent():
			apns.Headers["apns-push-type"] = "background"
			apns.Headers["apns-priority"] = "5"
			apns.Payload.APS.ContentAvailable = 1
		default:
			apns.Headers["apns-priority"] = "10"
			if p.Urgency() == PriorityNormal {
				apns.Headers["apns-priority"] = "5"
			}
			apns.Payload.APS.Sound = "default"
			if p.ImageURL != "" {
				apns.Payload.APS.MutableContent = 1
				apns.FCMOptions = &wireFCMOptions{Image: p.ImageURL}
			}
		}
		if p.TimeToLive > 0 {
			apns.Headers["apns-expiration"] = strconv.FormatInt(now.Add(p.TimeToLive).Unix(), 10)
		}
		if p.CollapseKeyName != "" {
			apns.Headers["apns-collapse-id"] = p.CollapseKeyName
		}
		message.APNS = apns
	}
	return json.Marshal(wireEnvelope{Message: message})
}

// ceilSeconds is a duration in whole seconds, rounded up, so a ttl of half a
// second is one second and not none.
func ceilSeconds(d time.Duration) int64 {
	seconds := int64(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	return seconds
}
