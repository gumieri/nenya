package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestIsSecretReference(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"env:OPENAI_API_KEY", true},
		{"file:/run/secrets/openai", true},
		{"file:secrets/openai.key", true},
		{"sk-abc123", false},
		{"env:", false},
		{"file:   ", false},
		{"envelope:weird", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsSecretReference(tc.value); got != tc.want {
			t.Fatalf("IsSecretReference(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// A resolved `env:` reference must be read once, in place; a missing or EMPTY
// source is an error, never a silent empty credential.
func TestResolveProviderKeyReferencesEnv(t *testing.T) {
	t.Setenv("NENYA_TEST_SECRET_REF", "sk-real-cred")
	secrets := &SecretsConfig{ProviderKeys: map[string]string{"anthropic": "env:NENYA_TEST_SECRET_REF"}}
	if err := ResolveProviderKeyReferences(secrets, "", DefaultSecretResolutionTimeout); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if secrets.ProviderKeys["anthropic"] != "sk-real-cred" {
		t.Fatalf("reference not resolved in place: %q", secrets.ProviderKeys["anthropic"])
	}

	t.Run("missing env var refuses the resolution", func(t *testing.T) {
		secrets := &SecretsConfig{ProviderKeys: map[string]string{"anthropic": "env:NENYA_TEST_SECRET_WITHDRAWN"}}
		err := ResolveProviderKeyReferences(secrets, "", DefaultSecretResolutionTimeout)
		if err == nil {
			t.Fatal("a missing environment variable must fail the resolution")
		}
		if secrets.ProviderKeys["anthropic"] == "" {
			t.Fatal("an unresolved reference must not become an empty credential")
		}
	})

	t.Run("empty env var refuses the resolution", func(t *testing.T) {
		t.Setenv("NENYA_TEST_SECRET_REVOKED", "   ")
		secrets := &SecretsConfig{ProviderKeys: map[string]string{"anthropic": "env:NENYA_TEST_SECRET_REVOKED"}}
		if err := ResolveProviderKeyReferences(secrets, "", DefaultSecretResolutionTimeout); err == nil {
			t.Fatal("an empty-or-whitespace env value must fail the resolution")
		}
	})
}

func TestResolveProviderKeyReferencesFile(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "keyfile")
	if err := os.WriteFile(credFile, []byte("  sk-file-cred\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	secrets := &SecretsConfig{ProviderKeys: map[string]string{"anthropic": "file:" + credFile}}
	if err := ResolveProviderKeyReferences(secrets, "", DefaultSecretResolutionTimeout); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if secrets.ProviderKeys["anthropic"] != "sk-file-cred" {
		t.Fatalf("want trimmed content, got %q", secrets.ProviderKeys["anthropic"])
	}

	t.Run("empty file refuses the resolution", func(t *testing.T) {
		empty := filepath.Join(dir, "empty")
		if err := os.WriteFile(empty, []byte("\n\t\n"), 0o600); err != nil {
			t.Fatalf("write empty: %v", err)
		}
		secrets := &SecretsConfig{ProviderKeys: map[string]string{"anthropic": "file:" + empty}}
		err := ResolveProviderKeyReferences(secrets, "", DefaultSecretResolutionTimeout)
		if err == nil {
			t.Fatal("an empty credential file must fail the resolution (fail closed)")
		}
	})

	t.Run("relative path resolves under the config root and rejects escape", func(t *testing.T) {
		rel := "keyfile"
		secrets := &SecretsConfig{ProviderKeys: map[string]string{"anthropic": "file:" + rel}}
		if err := ResolveProviderKeyReferences(secrets, dir, DefaultSecretResolutionTimeout); err != nil {
			t.Fatalf("relative under config root: %v", err)
		}
		if secrets.ProviderKeys["anthropic"] != "sk-file-cred" {
			t.Fatalf("relative reference content: %q", secrets.ProviderKeys["anthropic"])
		}

		escape := &SecretsConfig{ProviderKeys: map[string]string{"anthropic": "file:../../etc/passwd"}}
		if err := ResolveProviderKeyReferences(escape, dir, DefaultSecretResolutionTimeout); err == nil {
			t.Fatal("a file: path escaping the config root must be rejected")
		}
	})

	t.Run("relative path without a config root is rejected", func(t *testing.T) {
		secrets := &SecretsConfig{ProviderKeys: map[string]string{"anthropic": "file:orphan.key"}}
		if err := ResolveProviderKeyReferences(secrets, "", DefaultSecretResolutionTimeout); err == nil {
			t.Fatal("a relative file: reference in file mode must be rejected")
		}
	})

	t.Run("missing file refuses the resolution", func(t *testing.T) {
		secrets := &SecretsConfig{ProviderKeys: map[string]string{"anthropic": "file:/nonexistent/path/x.key"}}
		if err := ResolveProviderKeyReferences(secrets, "", DefaultSecretResolutionTimeout); err == nil {
			t.Fatal("a missing file must fail the resolution")
		}
	})
}

// A hanging credential source must never wedge the resolution (NENYA-166):
// a FIFO with no writer blocks open/read forever; the bounded read must take
// the timeout instead.
func TestReadSecretFileBoundedHangs(t *testing.T) {
	fifo := t.TempDir() + "/blocked"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	start := time.Now()
	if _, err := readSecretFileBounded(fifo, 150*time.Millisecond); err == nil {
		t.Fatal("a blocking source must be bounded")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the bound did not apply (took %s)", took)
	}

	// Closing the blocking reader goroutine's business: a writer unblocks it
	// after the timeout, so the fixture does not litter the environment.
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err == nil {
			_, _ = f.WriteString("late")
			_ = f.Close()
		}
	}()
}

// A rotated credential must be picked up on a re-load without the gateway
// needing to restart: the resolved value flows out of LoadSecretsBounded.
func TestLoadSecretsBoundedRotatesReference(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "openai.key")
	secretsFile := filepath.Join(dir, "secrets.json")
	doc := `{"client_token":"nk-client-0123456789abcdefg","provider_keys":{"openai":"file:openai.key"}}`
	if err := os.WriteFile(secretsFile, []byte(doc), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

	for _, value := range []string{"sk-rotation-1", "sk-rotation-2"} {
		if err := os.WriteFile(keyFile, []byte(value+"\n"), 0o600); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		secrets, err := LoadSecretsBounded(dir, DefaultSecretResolutionTimeout)
		if err != nil {
			t.Fatalf("load %s: %v", value, err)
		}
		if secrets.ProviderKeys["openai"] != value {
			t.Fatalf("rotation: want %q, got %q", value, secrets.ProviderKeys["openai"])
		}
	}
}

// A reference whose RESOLVED value is weak must refuse (re-validation after
// the resolution happened).
func TestLoadSecretsBoundedRefusesWeakResolvedCredential(t *testing.T) {
	t.Setenv("NENYA_TEST_WEAK_REF", " password ")
	dir := t.TempDir()
	secretsFile := filepath.Join(dir, "secrets.json")
	doc := `{"client_token":"nk-client-0123456789abcdefg","provider_keys":{"openai":"env:NENYA_TEST_WEAK_REF"}}`
	if err := os.WriteFile(secretsFile, []byte(doc), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	if _, err := LoadSecretsBounded(dir, DefaultSecretResolutionTimeout); err == nil {
		t.Fatal("a resolved weak credential must refuse the load")
	}
}

// The resolved values must pass validation; a REFERENCE VALUE ITSELF must not
// be strength-checked before resolution.
func TestLoadSecretsBoundedAcceptsStrongResolvedCredential(t *testing.T) {
	dir := t.TempDir()
	secretsFile := filepath.Join(dir, "secrets.json")
	doc := `{"client_token":"nk-client-0123456789abcdefg","provider_keys":{"openai":"env:NENYA_TEST_SECRET_STRONG"}}`
	if err := os.WriteFile(secretsFile, []byte(doc), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	t.Setenv("NENYA_TEST_SECRET_STRONG", "sk-strong-value-that-is-not-weak")
	secrets, err := LoadSecretsBounded(dir, DefaultSecretResolutionTimeout)
	if err != nil {
		t.Fatalf("strong resolved credential: %v", err)
	}
	if secrets.ProviderKeys["openai"] != "sk-strong-value-that-is-not-weak" {
		t.Fatalf("resolved value: %q", secrets.ProviderKeys["openai"])
	}
}

func TestEffectiveSecretResolutionTimeout(t *testing.T) {
	if got := (*Config)(nil).EffectiveSecretResolutionTimeout(); got != DefaultSecretResolutionTimeout {
		t.Fatalf("nil config: %v", got)
	}
	c := &Config{}
	if got := c.EffectiveSecretResolutionTimeout(); got != DefaultSecretResolutionTimeout {
		t.Fatalf("absent knob: %v", got)
	}
	v := 5
	c.Governance.SecretResolutionTimeoutSeconds = &v
	if got := c.EffectiveSecretResolutionTimeout(); got != 5*time.Second {
		t.Fatalf("configured: %v", got)
	}
	v = MaxSecretResolutionTimeoutSeconds * 10
	c.Governance.SecretResolutionTimeoutSeconds = &v
	if got := c.EffectiveSecretResolutionTimeout(); got != MaxSecretResolutionTimeoutSeconds*time.Second {
		t.Fatalf("cap: %v", got)
	}
}

func TestServeContinuityIssues(t *testing.T) {
	base := func() map[string]*Provider {
		return map[string]*Provider{
			"serving":  {Name: "serving", AuthStyle: "bearer", APIKey: "sk-old"},
			"keyless":  {Name: "keyless", AuthStyle: AuthStyleNone},
			"weakless": {Name: "weakless", AuthStyle: "bearer", APIKey: ""},
		}
	}
	cfgNames := func(names ...string) map[string]struct{} {
		out := make(map[string]struct{}, len(names))
		for _, n := range names {
			out[n] = struct{}{}
		}
		return out
	}

	t.Run("lost credential in a still-configured provider rejects", func(t *testing.T) {
		candidate := base()
		candidate["serving"] = &Provider{Name: "serving", AuthStyle: "bearer", APIKey: ""}
		lost := ServeContinuityIssues(base(), candidate, cfgNames("serving", "keyless", "weakless"))
		if len(lost) != 1 || lost[0] != "serving" {
			t.Fatalf("want [serving], got %v", lost)
		}
	})

	t.Run("removed provider resolves silently (deliberate removal)", func(t *testing.T) {
		candidate := base()
		delete(candidate, "serving")
		lost := ServeContinuityIssues(base(), candidate, cfgNames("keyless", "weakless"))
		if len(lost) != 0 {
			t.Fatalf("a deliberately removed provider is not a resolution failure: %v", lost)
		}
	})

	t.Run("still-configured but dropped from the resolved set rejects", func(t *testing.T) {
		candidate := base()
		delete(candidate, "serving")
		lost := ServeContinuityIssues(base(), candidate, cfgNames("serving", "keyless", "weakless"))
		if len(lost) != 1 || lost[0] != "serving" {
			t.Fatalf("a still-configured provider that failed to resolve must reject: %v", lost)
		}
	})

	t.Run("a deliberate switch to keyless is not a loss", func(t *testing.T) {
		candidate := base()
		candidate["serving"] = &Provider{Name: "serving", AuthStyle: AuthStyleNone, APIKey: ""}
		lost := ServeContinuityIssues(base(), candidate, cfgNames("serving", "keyless", "weakless"))
		if len(lost) != 0 {
			t.Fatalf("switching auth_style to none is a config change, not a secret failure: %v", lost)
		}
	})

	t.Run("no credential needed, no loss detected", func(t *testing.T) {
		candidate := base()
		candidate["keyless"].APIKey = ""
		lost := ServeContinuityIssues(base(), candidate, cfgNames("serving", "keyless", "weakless"))
		if len(lost) != 0 {
			t.Fatalf("a keyless provider is not a serving credential: %v", lost)
		}
	})
}
