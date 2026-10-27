package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Secret reference prefixes (NENYA-166). A provider key value starting with
// one of these is a live reference resolved at load and re-resolved on every
// SIGHUP reload, so rotating the underlying secret is a reload with no
// restart. Anything else is a literal credential.
const (
	// SecretRefEnvPrefix marks a provider key as an environment-variable
	// reference: "env:OPENAI_API_KEY".
	SecretRefEnvPrefix = "env:"
	// SecretRefFilePrefix marks a provider key as a file reference:
	// "file:/run/secrets/openai_key" (absolute) or a path under the config
	// root in directory mode.
	SecretRefFilePrefix = "file:"
)

// DefaultSecretResolutionTimeout bounds each secret-source read (NENYA-166):
// a hanging source — an open FIFO with no writer, a slow volume mount —
// must never wedge load, reload, or shutdown.
var DefaultSecretResolutionTimeout = 2 * time.Second

// MaxSecretResolutionTimeoutSeconds caps the operator's resolution bound.
const MaxSecretResolutionTimeoutSeconds = 60

// SecretReferenceError names the provider whose key reference failed, for a
// load/reload message that points at the exact fix.
type SecretReferenceError struct {
	// Provider is the secrets.provider_keys entry name.
	Provider string
	// Ref is the raw reference value (never the resolved credential).
	Ref string
	// Err is the cause (missing env var, missing/empty/hanging file).
	Err error
}

func (e SecretReferenceError) Error() string {
	return fmt.Sprintf("provider_keys[%q] reference %q: %v", e.Provider, e.Ref, e.Err)
}

// EffectiveSecretResolutionTimeout returns the resolution bound: the configured
// seconds when positive, DefaultSecretResolutionTimeout when absent, and the
// MaxSecretResolutionTimeoutSeconds cap for anything larger. Nil-safe.
func (c *Config) EffectiveSecretResolutionTimeout() time.Duration {
	n := 0
	if c != nil && c.Governance.SecretResolutionTimeoutSeconds != nil {
		n = *c.Governance.SecretResolutionTimeoutSeconds
	}
	if n <= 0 {
		return DefaultSecretResolutionTimeout
	}
	if n > MaxSecretResolutionTimeoutSeconds {
		return MaxSecretResolutionTimeoutSeconds * time.Second
	}
	return time.Duration(n) * time.Second
}

// IsSecretReference reports whether a provider key value is a well-formed
// env:/file: reference. A malformed prefix ("env:", "file:" with no body) is
// NOT one: the strength validation treats it as a (bad) literal.
func IsSecretReference(value string) bool {
	if strings.HasPrefix(value, SecretRefEnvPrefix) {
		return len(strings.TrimSpace(value[len(SecretRefEnvPrefix):])) > 0
	}
	if strings.HasPrefix(value, SecretRefFilePrefix) {
		return len(strings.TrimSpace(value[len(SecretRefFilePrefix):])) > 0
	}
	return false
}

// ResolveProviderKeyReferences resolves the env:/file: references in
// secrets.provider_keys in place (NENYA-166): at boot and on every reload, so
// a rotated secret is picked up without a restart. Fail closed: every
// resolution failure (missing/empty env var, missing/empty/hanging file) is an
// error — an unresolved reference must never silently become an empty
// credential, which is how a serving provider loses its key.
//
// configRoot is the directory-mode config root (empty in file mode); relative
// file: paths resolve under it and must not escape it. timeout bounds each
// file read.
func ResolveProviderKeyReferences(secrets *SecretsConfig, configRoot string, timeout time.Duration) error {
	if secrets == nil {
		return nil
	}
	var errs []error
	for _, name := range sortedProviderKeyNames(secrets) {
		value := secrets.ProviderKeys[name]
		if !IsSecretReference(value) {
			continue
		}
		resolved, err := resolveSecretReference(value, configRoot, timeout)
		if err != nil {
			// The raw reference is what the operator wrote (in secrets.json,
			// which they already hold): it names the fix without exposing the
			// resolved credential.
			errs = append(errs, SecretReferenceError{Provider: name, Ref: value, Err: err})
			continue
		}
		secrets.ProviderKeys[name] = resolved
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// resolveSecretReference resolves one reference to its concrete credential.
func resolveSecretReference(value, configRoot string, timeout time.Duration) (string, error) {
	switch {
	case strings.HasPrefix(value, SecretRefEnvPrefix):
		name := strings.TrimSpace(value[len(SecretRefEnvPrefix):])
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		if strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("environment variable %s is empty", name)
		}
		return strings.TrimSpace(v), nil

	case strings.HasPrefix(value, SecretRefFilePrefix):
		path := strings.TrimSpace(value[len(SecretRefFilePrefix):])
		path, err := resolveSecretFilePath(path, configRoot)
		if err != nil {
			return "", err
		}
		return readSecretFileBounded(path, timeout)

	default:
		// Not a reference: resolveSecretReference is only called on
		// IsSecretReference values; this is a programming error guard.
		return value, nil
	}
}

// containmentPrefix returns the path prefix every contained path must carry:
// root + separator, except the filesystem root itself (whose separator would
// otherwise double: root "/" + "/" = "//" rejects everything).
func containmentPrefix(root string) string {
	if root == string(filepath.Separator) {
		return root
	}
	return root + string(filepath.Separator)
}

// resolveSecretFilePath validates a file: target: absolute, or relative within
// the config root in directory mode (path traversal is rejected — NENYA-166
// keeps the repository's file-path security rule).
// Threat model: containment is verified on the resolved path, but the file
// is later re-opened BY NAME (readSecretFileBounded), so the guard is
// against config-directory accidents — traversal, links, misplacement — not
// a hostile local writer racing the path swap.
func resolveSecretFilePath(path, configRoot string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if configRoot == "" {
		return "", fmt.Errorf("relative file: reference %q requires a config directory (directory mode)", path)
	}
	joined, err := filepath.Abs(filepath.Join(configRoot, path))
	if err != nil {
		return "", fmt.Errorf("invalid file: path %q: %w", path, err)
	}
	root := CleanAbs(configRoot)
	// The containment compares use a root-relative prefix; a pathological
	// configRoot of "/" must not turn the prefix into "//" (which rejects
	// everything with a misleading message).
	prefix := containmentPrefix(root)
	if joined == root {
		return "", fmt.Errorf("file: path %q is the config directory itself", path)
	}
	if !strings.HasPrefix(joined, prefix) {
		return "", fmt.Errorf("file: path %q escapes the config directory %q", path, root)
	}
	// AUD-71 (NENYA-297): the lexical check above does not see SYMLINKS —
	// a link inside the config root can point anywhere on the filesystem.
	// Resolve the real path and re-verify the containment; fail closed on
	// resolution errors (a missing file fails the load downstream anyway).
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("file: path %q cannot be resolved: %w", path, err)
	}
	// The containment compare is resolved-vs-resolved: EvalSymlinks(joined)
	// resolves the ROOT component too, so comparing against the unresolved
	// root would falsely reject every internal reference when the config
	// root itself sits behind a symlink (review finding on NENYA-297). Best
	// effort: an unresolvable root keeps the lexical form (the lexical check
	// above already passed it).
	if resolvedRoot, rerr := filepath.EvalSymlinks(root); rerr == nil {
		root = CleanAbs(resolvedRoot)
	}
	prefix = containmentPrefix(root)
	// The root-itself check precedes the prefix test: under a "/" config root
	// every path carries the prefix, so a self-reference would otherwise be
	// accepted and only surface as an opaque read-a-directory error.
	if resolved == root {
		return "", fmt.Errorf("file: path %q resolves to the config directory itself", path)
	}
	if !strings.HasPrefix(resolved, prefix) {
		return "", fmt.Errorf("file: path %q resolves to %q which escapes the config directory %q", path, resolved, root)
	}
	return resolved, nil
}

// readSecretFileBounded reads one credential file under a deadline (NENYA-166):
// a hanging source (an open FIFO with no writer, a wedged volume) must never
// wedge boot, reload, or shutdown. On timeout the reading goroutine is not
// killed — a goroutine is consumed until the underlying read returns, then it
// finishes silently into a buffered channel; this is the deliberate
// fire-and-forget exception, and it is bounded by the timeout being short.
func readSecretFileBounded(path string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	type result struct {
		content []byte
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		content, err := os.ReadFile(path) //nolint:forbidigo // bounded by ctx below
		ch <- result{content: content, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("file read did not return within %s", timeout)
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("file read: %w", r.err)
		}
		content := strings.TrimSpace(string(r.content))
		if content == "" {
			return "", errors.New("file is empty")
		}
		return content, nil
	}
}

// sortedProviderKeyNames returns the provider_keys entries in stable order so
// resolution failures report deterministically.
func sortedProviderKeyNames(secrets *SecretsConfig) []string {
	names := make([]string, 0, len(secrets.ProviderKeys))
	for name := range secrets.ProviderKeys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
