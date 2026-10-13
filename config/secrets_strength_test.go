package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// strongToken is a credential-shaped value that passes every strength check.
const strongToken = "nk-0123456789abcdef0123456789abcdef01234567"

func testSecrets(clientToken string) *SecretsConfig {
	return &SecretsConfig{
		ClientToken:  clientToken,
		ProviderKeys: map[string]string{},
		ApiKeys:      map[string]ApiKey{},
	}
}

// refuseStrengthTable cases are run with the escape hatch explicitly cleared,
// so the suite is hermetic against a developer's NENYA_ALLOW_WEAK_SECRETS.
func TestValidateSecretsStrength_RefusesWeakClasses(t *testing.T) {
	t.Setenv(AllowWeakSecretsEnv, "")
	const source = "/etc/nenya/secrets.json"
	cases := []struct {
		name    string
		secrets *SecretsConfig
		wantIn  string
	}{
		{"client empty", testSecrets(""), "is empty"},
		{"client placeholder literal", testSecrets("test-client-token"), "publicly known placeholder"},
		{"client changeme literal", testSecrets("changeme-0000000000"), "refusing to start: client_token"},
		{"client below minimum", testSecrets("short-token-15"), "is too short"},
		{"client at boundary 15", testSecrets(strings.Repeat("x", 15)), "is too short"},
		{"client padded placeholder phrase", testSecrets("test-client-token-123456"), "contains a publicly known placeholder value"},
		{"client template", testSecrets("<your-token-here>"), "unexpanded template"},
		{"client unexpanded var", testSecrets("$CLIENT_TOKEN_REPLACE_ME"), "unexpanded shell variable"},
		{"client repeated lowercase char", testSecrets(strings.Repeat("a", 32)), "repeated single character"},
		{"client repeated uppercase char", testSecrets(strings.Repeat("A", 32)), "repeated single character"},
		{
			name: "provider key empty means keyless upstream",
			secrets: func() *SecretsConfig {
				s := testSecrets(strongToken)
				s.ProviderKeys["ollama"] = ""
				return s
			}(),
		},
		{
			name: "provider key short local shared secret",
			secrets: func() *SecretsConfig {
				s := testSecrets(strongToken)
				s.ProviderKeys["litellm-local"] = "sk-1234"
				return s
			}(),
		},
		{
			name: "provider key placeholder",
			secrets: func() *SecretsConfig {
				s := testSecrets(strongToken)
				s.ProviderKeys["openai"] = "Your-Api-Key-Here"
				return s
			}(),
			wantIn: "publicly known placeholder",
		},
		{
			name: "provider key padded placeholder phrase",
			secrets: func() *SecretsConfig {
				s := testSecrets(strongToken)
				s.ProviderKeys["openai"] = "sk-changeme-000000000000"
				return s
			}(),
			wantIn: "contains a publicly known placeholder value",
		},
		{
			name: "api key token placeholder",
			secrets: func() *SecretsConfig {
				s := testSecrets(strongToken)
				s.ApiKeys["ci"] = ApiKey{Token: "test-client-token", Enabled: true}
				return s
			}(),
			wantIn: `api_keys["ci"].token`,
		},
		{
			name: "enabled api key repeated char at floor",
			secrets: func() *SecretsConfig {
				s := testSecrets(strongToken)
				s.ApiKeys["k"] = ApiKey{Token: strings.Repeat("k", 16), Enabled: true}
				return s
			}(),
			wantIn: "repeated single character",
		},
		{
			name:    "client trivial word+digit",
			secrets: testSecrets("password12345678"),
			wantIn:  "trivially guessable",
		},
		{
			name: "disabled api key entry skipped",
			secrets: func() *SecretsConfig {
				s := testSecrets(strongToken)
				s.ApiKeys["off"] = ApiKey{Token: "test-client-token", Enabled: false}
				return s
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSecretsStrength(tc.secrets, source)
			if tc.wantIn == "" {
				if err != nil {
					t.Fatalf("expected pass, got refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected refusal, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantIn)
			}
			if !strings.Contains(err.Error(), source) {
				t.Errorf("error = %q, want source attribution %q", err.Error(), source)
			}
			if !strings.Contains(err.Error(), AllowWeakSecretsEnv) {
				t.Errorf("error = %q, want escape hatch %q mentioned", err.Error(), AllowWeakSecretsEnv)
			}
		})
	}
}

// TestValidateSecretsStrength_AcceptsStrongValues verifies that credential-
// shaped values boot, including the 16-rune minimum boundary.
func TestValidateSecretsStrength_AcceptsStrongValues(t *testing.T) {
	t.Setenv(AllowWeakSecretsEnv, "")
	s := testSecrets(strongToken)
	s.ProviderKeys["gemini"] = "AIzaSy-synthetic-key-0123456789abcdef"
	s.ApiKeys["ci"] = ApiKey{Token: "ci-token-0123456789abcdef", Roles: []string{"user"}, Enabled: true}
	if err := validateSecretsStrength(s, "/etc/nenya/secrets.json"); err != nil {
		t.Fatalf("strong secrets refused: %v", err)
	}

	s.ApiKeys["edge16"] = ApiKey{Token: "0123456789abcdef", Enabled: true}
	if err := validateSecretsStrength(s, "/etc/nenya/secrets.json"); err != nil {
		t.Fatalf("16-rune non-repeating enabled token refused: %v", err)
	}
}

// TestValidateSecretsStrength_EscapeHatch table-drives the documented
// NENYA_ALLOW_WEAK_SECRETS values: the enabling set waives every weak class;
// anything else must keep the control on (a regression to "set = disabled"
// would silently turn the check off for NENYA_ALLOW_WEAK_SECRETS=0).
func TestValidateSecretsStrength_EscapeHatch(t *testing.T) {
	weak := testSecrets("changeme-00000000")
	for _, v := range []string{"1", "true", "yes", "on", "TRUE", "Yes"} {
		t.Setenv(AllowWeakSecretsEnv, v)
		if err := validateSecretsStrength(weak, "/etc/nenya/secrets.json"); err != nil {
			t.Fatalf("hatch %q should have waived the refusal: %v", v, err)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "garbage"} {
		t.Setenv(AllowWeakSecretsEnv, v)
		if err := validateSecretsStrength(weak, "/etc/nenya/secrets.json"); err == nil {
			t.Fatalf("hatch %q must not disable the strength check", v)
		}
	}
}

// TestRecordSecrets_AttributesStrengthFailureToSource verifies the end-to-end
// path: a weak document recorded from a source file fails validation with the
// source named in both the error and SecretsResolution.FailedSource.
func TestRecordSecrets_AttributesStrengthFailureToSource(t *testing.T) {
	t.Setenv(AllowWeakSecretsEnv, "")
	res := SecretsResolution{}
	source := filepath.Join("etc", "nenya", "secrets.json")
	ok, err := recordSecrets(&res, source, testSecrets("changeme-00000000"))
	if ok {
		t.Fatal("weak secrets must not record")
	}
	if err == nil || !strings.Contains(err.Error(), source) {
		t.Fatalf("error = %v, want source attribution to %q", err, source)
	}
	if res.FailedSource != source {
		t.Errorf("FailedSource = %q, want %q", res.FailedSource, source)
	}

	// Provider-key and api_key branches attribute the same way.
	ok, err = recordSecrets(&res, source, func() *SecretsConfig {
		s := testSecrets(strongToken)
		s.ProviderKeys["openai"] = "your-api-key-here"
		return s
	}())
	if ok || err == nil || !strings.Contains(err.Error(), `provider_keys["openai"]`) {
		t.Fatalf("provider-key branch: ok=%v err=%v, want attributed refusal", ok, err)
	}
	ok, err = recordSecrets(&res, source, func() *SecretsConfig {
		s := testSecrets(strongToken)
		s.ApiKeys["bot"] = ApiKey{Token: "test-client-token", Enabled: true}
		return s
	}())
	if ok || err == nil || !strings.Contains(err.Error(), `api_keys["bot"].token`) {
		t.Fatalf("api-key branch: ok=%v err=%v, want attributed refusal", ok, err)
	}
}

// TestValidateConfigCredentialsStrength covers the config-file credential
// classes: placeholder/template literals in the legacy api_key and in
// accounts[].credential are refused, short-but-plausible local shared secrets
// are deliberately allowed (no length floor at config level), and the escape
// hatch disables the pass.
func TestValidateConfigCredentialsStrength(t *testing.T) {
	t.Setenv(AllowWeakSecretsEnv, "")
	mk := func(apiKey, credential string) *Config {
		return &Config{Providers: map[string]ProviderConfig{
			"openai": {APIKey: apiKey, Accounts: []AccountConfig{{ID: "a1", Credential: credential}}},
		}}
	}

	errs := ValidateConfigCredentialsStrength(mk("your-api-key-here", ""))
	if len(errs) != 1 || !strings.Contains(errs[0], `providers["openai"].api_key`) {
		t.Fatalf("errs = %v, want the legacy api_key placeholder refusal", errs)
	}

	errs = ValidateConfigCredentialsStrength(mk("", "<your-credential-here>"))
	if len(errs) != 1 || !strings.Contains(errs[0], `accounts[id="a1"].credential`) {
		t.Fatalf("errs = %v, want the account credential template refusal", errs)
	}

	// Short local shared secrets are allowed at config level (documented
	// policy: no length floor outside secrets.json).
	if errs := ValidateConfigCredentialsStrength(mk("", "sk-1234")); len(errs) != 0 {
		t.Fatalf("short local proxy credential must pass: %v", errs)
	}

	t.Setenv(AllowWeakSecretsEnv, "1")
	if errs := ValidateConfigCredentialsStrength(mk("changeme-000000000", "changeme-00000000")); len(errs) != 0 {
		t.Fatalf("escape hatch must disable the config-credential pass: %v", errs)
	}
}
