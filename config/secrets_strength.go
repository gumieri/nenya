package config

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// MinSecretLength is the minimum accepted length for the primary client_token
// and for api_keys entry tokens. It is the same numeric minimum ApiKey.Validate
// enforces for api_keys entries (RBAC §16; that check counts bytes, this one
// counts runes — identical for ASCII tokens). Provider keys have no length
// floor: local and proxy upstreams legitimately use short shared secrets, and
// they are checked for the placeholder/template classes only.
const MinSecretLength = 16

// AllowWeakSecretsEnv is the escape hatch for the secrets strength checks:
// setting it to 1/true/yes/on (case-insensitive) skips the strength pass.
// Structural validation (a required, non-empty client_token and well-formed
// api_keys entries) still applies. It exists for throwaway local experiments
// and the demo; production deployments must never set it.
const AllowWeakSecretsEnv = "NENYA_ALLOW_WEAK_SECRETS"

// weakSecretLiterals are publicly known credential values that must never
// serve traffic. Matched case-insensitively against trimmed values, exact
// only — these are short or generic words that must not substring-match
// inside a legitimate key.
var weakSecretLiterals = map[string]struct{}{
	"changeme":          {},
	"change-me":         {},
	"changeit":          {},
	"client-token":      {},
	"clienttoken":       {},
	"dummy":             {},
	"example":           {},
	"fake-token":        {},
	"insert-token-here": {},
	"nk-test":           {},
	"password":          {},
	"placeholder":       {},
	"replace-me":        {},
	"secret":            {},
	"sk-test":           {},
	"sk-xxx":            {},
	"sk-xxxx":           {},
	"test":              {},
	"test-token":        {},
	"testtoken":         {},
	"token":             {},
	"your-api-key":      {},
	"your-key-here":     {},
	"your_client_token": {},
	"your-client-token": {},
	"your-token-here":   {},
}

// weakSecretPhrases are unambiguous placeholder phrases matched as
// case-insensitive SUBSTRINGS, so padded variants like "changeme-0000" are
// caught. All are long and specific enough that a real key containing one is
// virtually impossible; generic words ("test", "token", "client-token")
// deliberately live in the exact-match table instead.
var weakSecretPhrases = []string{
	"changeme",
	"test-client-token",
	"test-token",
	"testtoken",
	"change-me",
	"fake-token",
	"insert-token-here",
	"placeholder",
	"replace-me",
	"your-api-key",
	"your-client-token",
	"your-key-here",
	"your-token-here",
	"your_client_token",
}

// weakSecretPatterns catch unexpanded templates and shell-command/variable
// substitution ANYWHERE in the value: real provider keys and tokens never
// contain "<…>" pairs or "$(...)" / "${...}" / "$VAR" sequences, while
// documented placeholder forms like "sk-<your-real-key>" or
// "deepseek-$DEEPSEEK_API_KEY" must refuse rather than boot.
var (
	weakTemplatePattern   = regexp.MustCompile(`<[^<>]*>`)
	weakShellVarPattern   = regexp.MustCompile(`\$\([^)]*\)|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\$[A-Za-z_][A-Za-z0-9_]*`)
	weakWordDigitsPattern = regexp.MustCompile(`^(?:password|passwd|admin|user|dev|test|secret)[0-9]+$`)
)

var warnWeakSecretsOnce sync.Once

// allowWeakSecrets reports whether the operator explicitly disabled the
// strength checks via the environment escape hatch, emitting a one-time
// stderr warning when it is active (the check runs on boot, reload, and the
// inspection commands, so the warning must not repeat per call; stderr is
// deliberate — the config package has no injected logger and describe/secret
// get write JSON to stdout).
func allowWeakSecrets() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AllowWeakSecretsEnv))) {
	case "1", "true", "yes", "on":
		warnWeakSecretsOnce.Do(func() {
			fmt.Fprintln(os.Stderr, "WARNING: weak-credential startup checks are disabled (NENYA_ALLOW_WEAK_SECRETS is set) — never run this in production")
		})
		return true
	}
	return false
}

// weakSecretReason classifies a secrets-document credential with the full
// rule set: emptiness, placeholder literals/phrases, unexpanded
// templates/variables, the MinSecretLength rune floor, repeated single
// characters, and trivial word+digit values.
func weakSecretReason(value string) string {
	return weakSecretReasonDepth(value, MinSecretLength)
}

// weakSecretReasonPatternOnly classifies a config-file credential with the
// placeholder/template/empty/repeat classes but NO length floor and no
// word+digit rule: local and proxy upstreams legitimately use short shared
// secrets, so only unambiguous placeholder forms refuse.
func weakSecretReasonPatternOnly(value string) string {
	return weakSecretReasonDepth(value, 0)
}

// weakSecretReasonDepth is the shared classifier. minLength is the enforced
// floor in runes (0 disables it). Deliberate limits: literal matching is
// exact (case-insensitive) plus the weakSecretPhrases substring set; there is
// no entropy, dictionary, or homoglyph detection beyond the word+digit rule —
// low-entropy values like "ababab..." are accepted limitations.
func weakSecretReasonDepth(value string, minLength int) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "is empty"
	}
	lower := strings.ToLower(trimmed)
	// Separator-normalized form so underscore variants ("change_me",
	// "your_api_key") match the hyphenated tables.
	normed := strings.ReplaceAll(lower, "_", "-")
	if _, ok := weakSecretLiterals[lower]; ok {
		return "is a publicly known placeholder value"
	}
	if _, ok := weakSecretLiterals[normed]; ok {
		return "is a publicly known placeholder value"
	}
	if weakTemplatePattern.MatchString(trimmed) {
		return "contains an unexpanded template placeholder"
	}
	if weakShellVarPattern.MatchString(trimmed) {
		return "contains an unexpanded shell variable or command substitution"
	}
	for _, phrase := range weakSecretPhrases {
		if strings.Contains(lower, phrase) || strings.Contains(normed, phrase) {
			return "contains a publicly known placeholder value"
		}
	}
	if minLength > 0 {
		if weakWordDigitsPattern.MatchString(lower) {
			return "is a trivially guessable word+digit value"
		}
		if runes := utf8.RuneCountInString(trimmed); runes < minLength {
			return fmt.Sprintf("is too short (%d characters; minimum %d)", runes, minLength)
		}
	}
	first, _ := utf8.DecodeRuneInString(lower)
	if strings.Count(lower, string(first)) == utf8.RuneCountInString(lower) {
		return "is a repeated single character"
	}
	return ""
}

// credentialLabel formats a credential name for refusal messages.
func credentialLabel(kind, name string) string {
	if name == "" {
		return kind
	}
	return fmt.Sprintf("%s[%q]", kind, name)
}

// refuseWeakSecret formats a fail-closed startup error naming the credential,
// the source it was loaded from, the remediation clause for that credential
// kind, and the documented escape hatch.
func refuseWeakSecret(cred, source, reason, remediation string) error {
	return fmt.Errorf(
		"refusing to start: %s %s (source: %s); %s (or set %s=1 to override, dev only)",
		cred, reason, source, remediation, AllowWeakSecretsEnv)
}

// validateSecretsStrength refuses boot on credentials that are empty, publicly
// known placeholders, unexpanded templates/variables, repeated single
// characters, trivial word+digit values, or below MinSecretLength: the primary
// client_token and every api_key token get the full rule set; provider keys
// are checked for the placeholder/template/repeated-character classes only
// (empty means "no key for this upstream", matching the gateway's treatment,
// and short shared secrets are legitimate for local/proxy upstreams).
// api_keys entries additionally enforce roles/expiry via ApiKey.Validate
// (which runs separately; disabled entries are skipped here — mergeSecrets
// never uses them). All entries are checked
// regardless of whether a provider is referenced: a stale placeholder in a
// long-lived secrets file should be cleaned, not silently tolerated. The
// NENYA_ALLOW_WEAK_SECRETS env escape hatch disables this pass for throwaway
// local runs (structural validation still applies). source names the secrets
// source for error attribution — a single file, or the merged secrets.d/
// directory when the value came from a merge. secrets must be non-nil.
func validateSecretsStrength(secrets *SecretsConfig, source string) error {
	if allowWeakSecrets() {
		return nil
	}
	if reason := weakSecretReason(secrets.ClientToken); reason != "" {
		return refuseWeakSecret("client_token", source, reason, "generate a strong value with `nenya secret set --client-token`")
	}
	for _, name := range slices.Sorted(maps.Keys(secrets.ProviderKeys)) {
		key := secrets.ProviderKeys[name]
		if strings.TrimSpace(key) == "" {
			continue // empty provider key = keyless upstream (no credential sent)
		}
		if IsSecretReference(key) {
			// A well-formed env:/file: reference is not a credential
			// (NENYA-166): its RESOLVED value is strength-checked at the
			// LoadSecrets boundary, so an env var holding a weak secret is
			// still refused. This pass only guards the reference shape.
			continue
		}
		if reason := weakSecretReasonPatternOnly(key); reason != "" {
			return refuseWeakSecret(credentialLabel("provider_keys", name), source, reason, "generate a strong value with `nenya secret set --provider <name>`")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(secrets.ApiKeys)) {
		if !secrets.ApiKeys[name].Enabled {
			continue
		}
		if reason := weakSecretReason(secrets.ApiKeys[name].Token); reason != "" {
			return refuseWeakSecret(credentialLabel("api_keys", name)+".token", source, reason, "edit the token in the secrets document")
		}
	}
	return nil
}

// ValidateClientTokenStrength reports whether an explicit client_token would
// be refused at boot. `nenya secret set --client-token` uses it so the writer
// never produces a document the gateway rejects. Honors
// NENYA_ALLOW_WEAK_SECRETS: when the hatch is set, the writer accepts the same
// values the loader (with the same hatch set) accepts.
func ValidateClientTokenStrength(token string) error {
	if allowWeakSecrets() {
		return nil
	}
	if reason := weakSecretReason(token); reason != "" {
		return fmt.Errorf("client_token %s", reason)
	}
	return nil
}

// ValidateProviderKeyStrength reports whether an explicit provider key would
// be refused at boot (placeholder/template/empty classes only — no length
// floor). `nenya secret set --provider` uses it so the writer never produces a
// document the gateway rejects.
func ValidateProviderKeyStrength(key string) error {
	if allowWeakSecrets() {
		return nil
	}
	if strings.TrimSpace(key) == "" {
		return nil // empty = keyless upstream, matching the loader
	}
	if reason := weakSecretReasonPatternOnly(key); reason != "" {
		return fmt.Errorf("provider key %s", reason)
	}
	return nil
}

// ValidateConfigCredentialsStrength applies the placeholder/template/empty
// classes (deliberately NOT the length floor: local and proxy upstreams
// legitimately use short shared secrets) to credentials carried by the config
// file itself: the legacy providers.<name>.api_key and
// providers.<name>.accounts[].credential. Returns one error per weak value.
// Called from the boot path (run) and the full validation suite, so a
// placeholder credential in config.json refuses startup exactly like a weak
// secrets document.
func ValidateConfigCredentialsStrength(cfg *Config) []string {
	if allowWeakSecrets() {
		return nil
	}
	var errs []string
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[name]
		if strings.TrimSpace(p.APIKey) != "" {
			if reason := weakSecretReasonPatternOnly(p.APIKey); reason != "" {
				errs = append(errs, fmt.Sprintf("providers[%q].api_key %s (config file); fix the value or set %s=1 to override (dev only)", name, reason, AllowWeakSecretsEnv))
			}
		}
		for _, acct := range p.Accounts {
			if strings.TrimSpace(acct.Credential) == "" {
				continue
			}
			if reason := weakSecretReasonPatternOnly(acct.Credential); reason != "" {
				errs = append(errs, fmt.Sprintf("providers[%q].accounts[id=%q].credential %s (config file); fix the value or set %s=1 to override (dev only)", name, acct.ID, reason, AllowWeakSecretsEnv))
			}
		}
	}
	return errs
}
