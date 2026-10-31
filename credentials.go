package fcm

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// The environment variables CredentialsFromEnv reads, in the order it asks.
const (
	// EnvCredentialsJSON holds the service account JSON itself.
	EnvCredentialsJSON = "FCM_CREDENTIALS_JSON"
	// EnvCredentialsBase64 holds the service account JSON encoded in standard
	// base64, for deployment systems that mangle a multi-line value.
	EnvCredentialsBase64 = "FCM_CREDENTIALS_BASE64"
	// EnvCredentialsFile holds the path of the service account JSON file.
	EnvCredentialsFile = "FCM_CREDENTIALS_FILE"
	// EnvGoogleCredentials is the variable Google's own libraries read for the
	// same path. It is the last resort, so a deployment that already sets it
	// needs nothing new.
	EnvGoogleCredentials = "GOOGLE_APPLICATION_CREDENTIALS"
)

// redacted is what stands for a secret wherever a secret could be printed.
const redacted = "[redacted]"

// maxCredentialBytes bounds a credential file or value. A service account key
// is about 2.4 KB; anything past this is not one.
const maxCredentialBytes = 64 << 10

// Credentials is the Firebase service account the module signs in with.
//
// The private key is held in a value that prints, logs and encodes as
// [redacted] in every way Go can ask it to: String, GoString, every fmt verb,
// slog, JSON and text encoding. The accessors that remain answer facts that
// are not secret: the project and the account's address.
//
// The zero value is empty; [Credentials.IsZero] says so.
type Credentials struct {
	account *serviceAccount
}

// serviceAccount is the parsed key. It is reached only through Credentials and
// only by the client that signs with it.
type serviceAccount struct {
	projectID    string
	clientEmail  string
	privateKeyID string
	tokenURI     string
	key          *rsa.PrivateKey
}

// serviceAccountFile is the JSON Google issues for a service account.
type serviceAccountFile struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

// DefaultTokenURI is where a service account trades its signed assertion for an
// access token when its file names no other address.
const DefaultTokenURI = "https://oauth2.googleapis.com/token"

// ParseCredentials reads the JSON of a Firebase service account.
//
// Its errors name the field that is wrong and never carry any of the content:
// a key pasted into the wrong variable must not end up in a boot log.
func ParseCredentials(raw []byte) (Credentials, error) {
	if len(raw) == 0 {
		return Credentials{}, errors.New("fcm: the service account JSON is empty")
	}
	if len(raw) > maxCredentialBytes {
		return Credentials{}, errors.New("fcm: the service account JSON is too large to be one")
	}
	var file serviceAccountFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return Credentials{}, errors.New("fcm: the service account is not valid JSON")
	}
	if file.Type != "service_account" {
		return Credentials{}, errors.New(`fcm: the service account JSON has no "type": "service_account"`)
	}
	for _, field := range []struct{ name, value string }{
		{"project_id", file.ProjectID},
		{"client_email", file.ClientEmail},
		{"private_key", file.PrivateKey},
	} {
		if strings.TrimSpace(field.value) == "" {
			return Credentials{}, fmt.Errorf("fcm: the service account JSON has no %q", field.name)
		}
	}
	key, err := parsePrivateKey(file.PrivateKey)
	if err != nil {
		return Credentials{}, err
	}
	tokenURI := strings.TrimSpace(file.TokenURI)
	if tokenURI == "" {
		tokenURI = DefaultTokenURI
	}
	if err := checkEndpoint("token_uri", tokenURI); err != nil {
		return Credentials{}, err
	}
	return Credentials{account: &serviceAccount{
		projectID:    strings.TrimSpace(file.ProjectID),
		clientEmail:  strings.TrimSpace(file.ClientEmail),
		privateKeyID: file.PrivateKeyID,
		tokenURI:     tokenURI,
		key:          key,
	}}, nil
}

// parsePrivateKey reads the PEM block of a service account: PKCS#8, as Google
// issues it, or PKCS#1. Its errors never include the block.
func parsePrivateKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New(`fcm: the service account "private_key" is not a PEM block`)
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New(`fcm: the service account "private_key" is not an RSA key`)
		}
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New(`fcm: the service account "private_key" cannot be read`)
}

// CredentialsFromFile reads a service account JSON file.
func CredentialsFromFile(path string) (Credentials, error) {
	if strings.TrimSpace(path) == "" {
		return Credentials{}, errors.New("fcm: the service account file path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("fcm: the service account file %q cannot be read", path)
	}
	if info.Size() > maxCredentialBytes {
		return Credentials{}, fmt.Errorf("fcm: the file %q is too large to be a service account", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("fcm: the service account file %q cannot be read", path)
	}
	return ParseCredentials(raw)
}

// CredentialsFromEnv reads the service account from the environment: the JSON
// in FCM_CREDENTIALS_JSON, or in base64 in FCM_CREDENTIALS_BASE64, or the path
// of a file in FCM_CREDENTIALS_FILE or GOOGLE_APPLICATION_CREDENTIALS, in that
// order. The application passes the lookup, os.LookupEnv in production, so the
// module never reads the environment on its own.
func CredentialsFromEnv(lookup func(key string) (string, bool)) (Credentials, error) {
	if lookup == nil {
		return Credentials{}, errors.New("fcm: CredentialsFromEnv needs a lookup function, such as os.LookupEnv")
	}
	set := func(key string) (string, bool) {
		value, ok := lookup(key)
		value = strings.TrimSpace(value)
		return value, ok && value != ""
	}
	if value, ok := set(EnvCredentialsJSON); ok {
		return ParseCredentials([]byte(value))
	}
	if value, ok := set(EnvCredentialsBase64); ok {
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return Credentials{}, fmt.Errorf("fcm: %s is not valid base64", EnvCredentialsBase64)
		}
		return ParseCredentials(raw)
	}
	if value, ok := set(EnvCredentialsFile); ok {
		return CredentialsFromFile(value)
	}
	if value, ok := set(EnvGoogleCredentials); ok {
		return CredentialsFromFile(value)
	}
	return Credentials{}, fmt.Errorf("fcm: no service account in the environment: set %s, %s or %s",
		EnvCredentialsJSON, EnvCredentialsBase64, EnvCredentialsFile)
}

// IsZero reports whether the credentials are empty.
func (c Credentials) IsZero() bool { return c.account == nil }

// ProjectID is the Firebase project messages are sent to.
func (c Credentials) ProjectID() string {
	if c.account == nil {
		return ""
	}
	return c.account.projectID
}

// ClientEmail is the address of the service account. It identifies the
// account and is not a secret.
func (c Credentials) ClientEmail() string {
	if c.account == nil {
		return ""
	}
	return c.account.clientEmail
}

// String describes the credentials without the key.
func (c Credentials) String() string {
	if c.account == nil {
		return "fcm.Credentials{}"
	}
	return fmt.Sprintf("fcm.Credentials{project:%s account:%s key:%s}", c.account.projectID, c.account.clientEmail, redacted)
}

// GoString is what %#v prints.
func (c Credentials) GoString() string { return c.String() }

// Format answers every fmt verb with String, so no verb reaches the fields.
func (c Credentials) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// LogValue is what slog records.
func (c Credentials) LogValue() slog.Value {
	if c.account == nil {
		return slog.StringValue("")
	}
	return slog.GroupValue(
		slog.String("project", c.account.projectID),
		slog.String("account", c.account.clientEmail),
		slog.String("key", redacted),
	)
}

// MarshalJSON encodes the credentials without the key.
func (c Credentials) MarshalJSON() ([]byte, error) {
	if c.account == nil {
		return []byte("null"), nil
	}
	return json.Marshal(map[string]string{
		"project_id":   c.account.projectID,
		"client_email": c.account.clientEmail,
		"private_key":  redacted,
	})
}

// MarshalText encodes the credentials without the key.
func (c Credentials) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// checkEndpoint refuses an address the module would send credentials or
// messages to over a channel that can be read: anything but https, except a
// loopback host, which is how a local emulator or a test server is reached.
func checkEndpoint(field, raw string) error {
	u, err := parseURL(raw)
	if err != nil {
		return fmt.Errorf("fcm: %s is not an absolute http or https address", field)
	}
	if u.Scheme == "https" {
		return nil
	}
	if isLoopback(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("fcm: %s has to be https: credentials and messages do not travel in clear", field)
}
