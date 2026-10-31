package unit_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"bytes"

	fcm "github.com/hyz-is/arandu-fcm"
)

var (
	keyOnce sync.Once
	pemKey  string
	pemBody string
)

// serviceAccountJSON is a service account with a key generated for the run.
func serviceAccountJSON(t *testing.T, mutate func(map[string]string)) []byte {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			panic(err)
		}
		pemKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		pemBody = base64.StdEncoding.EncodeToString(der)
	})
	fields := map[string]string{
		"type": "service_account", "project_id": "demo-project", "private_key_id": "kid-1",
		"private_key": pemKey, "client_email": "push@demo-project.iam.gserviceaccount.com",
	}
	if mutate != nil {
		mutate(fields)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTheCredentialsAreReadFromAServiceAccount(t *testing.T) {
	t.Parallel()

	c, err := fcm.ParseCredentials(serviceAccountJSON(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.IsZero() || c.ProjectID() != "demo-project" || c.ClientEmail() != "push@demo-project.iam.gserviceaccount.com" {
		t.Fatalf("credentials = %s", c)
	}
	if !(fcm.Credentials{}).IsZero() {
		t.Fatal("the zero credentials are not zero")
	}
}

func TestTheCredentialsRefuseWhatIsNotAServiceAccount(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string][]byte{
		"empty":       nil,
		"not json":    []byte("not json at all"),
		"wrong type":  serviceAccountJSON(t, func(f map[string]string) { f["type"] = "authorized_user" }),
		"no project":  serviceAccountJSON(t, func(f map[string]string) { delete(f, "project_id") }),
		"no email":    serviceAccountJSON(t, func(f map[string]string) { delete(f, "client_email") }),
		"no key":      serviceAccountJSON(t, func(f map[string]string) { delete(f, "private_key") }),
		"key not pem": serviceAccountJSON(t, func(f map[string]string) { f["private_key"] = "SECRET-KEY-MATERIAL" }),
		"key garbage": serviceAccountJSON(t, func(f map[string]string) {
			f["private_key"] = "-----BEGIN PRIVATE KEY-----\nU0VDUkVULUtFWS1NQVRFUklBTA==\n-----END PRIVATE KEY-----\n"
		}),
		"plain http token uri": serviceAccountJSON(t, func(f map[string]string) { f["token_uri"] = "http://evil.example.com/token" }),
		"relative token uri":   serviceAccountJSON(t, func(f map[string]string) { f["token_uri"] = "/token" }),
	} {
		_, err := fcm.ParseCredentials(raw)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		// An error never repeats what it was given: a key pasted into the
		// wrong variable must not end up in a boot log.
		for _, leaked := range []string{"SECRET-KEY-MATERIAL", "U0VDUkVULUtFWS1NQVRFUklBTA", pemBody} {
			if leaked != "" && strings.Contains(err.Error(), leaked) {
				t.Errorf("%s: the error repeats the input: %v", name, err)
			}
		}
	}
}

func TestALoopbackTokenAddressMayBePlainHTTP(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{"http://127.0.0.1:8080/token", "http://localhost:9099/token", "https://oauth2.example.com/token"} {
		if _, err := fcm.ParseCredentials(serviceAccountJSON(t, func(f map[string]string) { f["token_uri"] = uri })); err != nil {
			t.Errorf("%s was refused: %v", uri, err)
		}
	}
}

// TestTheCredentialsNeverPrintLogOrEncodeTheKey holds the rule in every way Go
// can ask a value to show itself.
func TestTheCredentialsNeverPrintLogOrEncodeTheKey(t *testing.T) {
	t.Parallel()

	c, err := fcm.ParseCredentials(serviceAccountJSON(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	cfg := fcm.Config{Credentials: c, Key: []byte("APPLICATION-KEY-0123456789abcdef")}
	out := fcm.Outgoing{Token: "REGISTRATION-TOKEN-12345", Platform: fcm.PlatformIOS, Push: fcm.NewPush().Title("t")}

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("credentials", "c", c, "cfg", cfg, "out", out)
	logger.Info("text", "c", fmt.Sprint(c))

	forms := map[string]string{
		"String":      c.String(),
		"%v":          fmt.Sprintf("%v", c),
		"%+v":         fmt.Sprintf("%+v", c),
		"%#v":         fmt.Sprintf("%#v", c),
		"%s":          fmt.Sprintf("%s", c),
		"%q":          fmt.Sprintf("%q", c),
		"%v pointer":  fmt.Sprintf("%v", &c),
		"%+v config":  fmt.Sprintf("%+v", cfg),
		"%#v config":  fmt.Sprintf("%#v", cfg),
		"config":      cfg.String(),
		"%v outgoing": fmt.Sprintf("%v %+v %#v", out, out, out),
		"slog":        logged.String(),
	}
	for name, encode := range map[string]func() ([]byte, error){
		"json":          func() ([]byte, error) { return json.Marshal(c) },
		"json config":   func() ([]byte, error) { return json.Marshal(cfg) },
		"json outgoing": func() ([]byte, error) { return json.Marshal(out) },
		"text":          func() ([]byte, error) { return c.MarshalText() },
		"json in map":   func() ([]byte, error) { return json.Marshal(map[string]any{"credentials": c}) },
		"json in slice": func() ([]byte, error) { return json.Marshal([]fcm.Credentials{c}) },
	} {
		raw, err := encode()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		forms[name] = string(raw)
	}

	secrets := []string{pemBody, "BEGIN PRIVATE KEY", "APPLICATION-KEY-0123456789abcdef", "REGISTRATION-TOKEN-12345", "kid-1"}
	for name, text := range forms {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s shows a secret (%q): %s", name, secret, text)
			}
		}
		if text == "" {
			t.Errorf("%s printed nothing", name)
		}
	}
	if !strings.Contains(forms["String"], "demo-project") || !strings.Contains(forms["String"], "[redacted]") {
		t.Errorf("String = %s, want the project and a marker where the key is", forms["String"])
	}
}

func TestTheCredentialsAreReadFromTheEnvironment(t *testing.T) {
	t.Parallel()

	raw := serviceAccountJSON(t, nil)
	file := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	other := serviceAccountJSON(t, func(f map[string]string) { f["project_id"] = "other-project" })
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) { v, ok := env[key]; return v, ok }
	}

	cases := []struct {
		name    string
		env     map[string]string
		project string
	}{
		{"json", map[string]string{fcm.EnvCredentialsJSON: string(raw)}, "demo-project"},
		{"base64", map[string]string{fcm.EnvCredentialsBase64: base64.StdEncoding.EncodeToString(raw)}, "demo-project"},
		{"file", map[string]string{fcm.EnvCredentialsFile: file}, "demo-project"},
		{"google variable", map[string]string{fcm.EnvGoogleCredentials: file}, "demo-project"},
		{"json wins over the file", map[string]string{fcm.EnvCredentialsJSON: string(other), fcm.EnvCredentialsFile: file}, "other-project"},
		{"our file wins over google's", map[string]string{fcm.EnvCredentialsFile: file, fcm.EnvGoogleCredentials: "/nowhere"}, "demo-project"},
		{"blank variables are unset", map[string]string{fcm.EnvCredentialsJSON: "  ", fcm.EnvCredentialsFile: file}, "demo-project"},
	}
	for _, tc := range cases {
		c, err := fcm.CredentialsFromEnv(lookup(tc.env))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if c.ProjectID() != tc.project {
			t.Errorf("%s: project %q, want %q", tc.name, c.ProjectID(), tc.project)
		}
	}

	if _, err := fcm.CredentialsFromEnv(lookup(nil)); err == nil || !strings.Contains(err.Error(), fcm.EnvCredentialsJSON) {
		t.Errorf("nothing set answered %v, want the names of the variables", err)
	}
	if _, err := fcm.CredentialsFromEnv(nil); err == nil {
		t.Error("a nil lookup was accepted")
	}
	if _, err := fcm.CredentialsFromEnv(lookup(map[string]string{fcm.EnvCredentialsBase64: "***"})); err == nil {
		t.Error("invalid base64 was accepted")
	}
	if _, err := fcm.CredentialsFromEnv(lookup(map[string]string{fcm.EnvCredentialsFile: filepath.Join(t.TempDir(), "missing.json")})); err == nil {
		t.Error("a missing file was accepted")
	}
}
