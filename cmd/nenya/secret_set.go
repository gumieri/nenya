package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nenya/config"
)

// handleSecret dispatches the `nenya secret` subcommands (CONTRACT.md §4.7):
// the secrets single writer (`set`) and reader (`get`). It reports whether it
// handled the invocation.
func handleSecret(w, errW io.Writer, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "secret" {
		return false, nil
	}
	if len(args) >= 2 && args[1] == "set" {
		return handleSecretSet(w, errW, args)
	}
	if len(args) >= 2 && args[1] == "get" {
		return handleSecretGet(w, errW, args)
	}
	_, _ = fmt.Fprintln(errW, secretUsage)
	return true, errUsage
}

// handleSecretSet implements `nenya secret set`: the secrets single writer.
func handleSecretSet(w, errW io.Writer, args []string) (bool, error) {
	fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
	// A parse error can echo a flag value, and this command handles secrets, so
	// the flag package's output is discarded and usage is printed explicitly.
	fs.SetOutput(io.Discard)
	provider := fs.String("provider", "", "Provider name whose key is set")
	clientToken := fs.Bool("client-token", false, "Set (or generate) the client token")
	configDir, configFile := addConfigRootFlags(fs)
	if err := parseCommandFlags(fs, args[2:]); err != nil {
		_, _ = fmt.Fprintln(errW, secretSetUsage)
		return true, err
	}

	if *clientToken && *provider != "" {
		_, _ = fmt.Fprintln(errW, "usage: --provider and --client-token are mutually exclusive")
		return true, errUsage
	}

	target, targetErr := secretTargetPath(effectiveConfigPaths(*configDir, *configFile))
	if targetErr != nil {
		return true, targetErr
	}

	if setErr := applySecretSet(target, *clientToken, *provider, fs.Args()); setErr != nil {
		if errors.Is(setErr, errUsage) {
			_, _ = fmt.Fprintln(errW, secretSetUsage)
		}
		return true, setErr
	}

	_, outErr := fmt.Fprintln(w, target)
	return true, outErr
}

// applySecretSet routes a parsed `secret set` invocation to the matching
// writer.
func applySecretSet(target string, clientToken bool, provider string, rest []string) error {
	switch {
	case clientToken:
		return applyClientToken(target, rest)
	case provider != "":
		return applyProviderKey(target, provider, rest)
	default:
		return errUsage
	}
}

// secretSetUsage is the `secret set` usage line.
const secretSetUsage = "usage: nenya secret set [--config-dir <dir> | --config <file>] --provider <name> <api-key> | --client-token [<token>]"

// secretUsage is the `secret` subtree usage line, printed for an unknown
// subcommand.
const secretUsage = secretSetUsage + "\n       " + secretGetUsage

// applyClientToken sets client_token from rest[0], or generates one when no
// value is given. A malformed invocation returns errUsage.
func applyClientToken(target string, rest []string) error {
	if len(rest) > 1 {
		return errUsage
	}
	token := ""
	if len(rest) == 1 {
		token = rest[0]
	} else {
		generated, err := generateClientToken()
		if err != nil {
			return fmt.Errorf("generate client token: %w", err)
		}
		token = generated
	}
	if token == "" {
		return errUsage
	}
	return setSecretValue(target, "client_token", "", token)
}

// applyProviderKey sets provider_keys[provider] from rest[0].
func applyProviderKey(target, provider string, rest []string) error {
	if len(rest) != 1 {
		return errUsage
	}
	return setSecretValue(target, "provider", provider, rest[0])
}

// secretTargetPath returns the secrets file `secret set` writes. The systemd
// credential sources have higher priority than the file sources (CONTRACT.md
// §6.1); writing elsewhere while they are active would be shadowed, so the
// command fails closed and asks the operator to manage credentials via systemd.
//
// Precedence for the write target:
//  1. NENYA_SECRETS_DIR, when set — <dir>/secrets.json (unchanged).
//  2. Directory mode — <config-root>/secrets.json, the file the shipped unit
//     wires via LoadCredential and the loader also searches (CONTRACT.md §6.1
//     source 5), instead of the /run/secrets/nenya default the unit shadows.
//  3. File mode — /run/secrets/nenya/secrets.json (no config root to prefer).
//
// Every branch fails closed rather than writing a file a higher-priority source
// would shadow: systemd credentials (§6.1 sources 1-2), and the default merge
// directory /run/secrets/nenya (§6.1 source 4) when it is populated.
func secretTargetPath(paths configPaths) (string, error) {
	if credDir := os.Getenv("CREDENTIALS_DIRECTORY"); credDir != "" {
		if _, err := os.Stat(filepath.Join(credDir, "secrets")); err == nil {
			return "", fmt.Errorf("active secrets source is the systemd credential %s/secrets; manage it via systemd (or set NENYA_SECRETS_DIR to write a file instead)", credDir)
		}
		if entries, err := os.ReadDir(filepath.Join(credDir, "secrets.d")); err == nil && len(entries) > 0 {
			return "", fmt.Errorf("active secrets source is the systemd credential directory %s/secrets.d; manage it via systemd (or set NENYA_SECRETS_DIR to write a file instead)", credDir)
		}
	}

	if dir := os.Getenv("NENYA_SECRETS_DIR"); dir != "" {
		dir = absOrSelf(dir)
		// The loader accepts NENYA_SECRETS_DIR as a file or a directory; write
		// to a file target directly instead of creating a bogus directory.
		if info, err := os.Stat(dir); err == nil && !info.IsDir() {
			return dir, nil
		}
		return filepath.Join(dir, "secrets.json"), nil
	}
	if paths.file == "" {
		target := filepath.Join(absOrSelf(paths.dir), "secrets.json")
		if filepath.Dir(target) != config.DefaultSecretsDir && defaultSecretsDirPopulated() {
			return "", fmt.Errorf("active secrets source is %s (source 4); writing %s would be shadowed — remove %s/*.json or set NENYA_SECRETS_DIR to write a file instead", config.DefaultSecretsDir, target, config.DefaultSecretsDir)
		}
		return target, nil
	}
	return filepath.Join(config.DefaultSecretsDir, "secrets.json"), nil
}

// defaultSecretsDirPopulated reports whether the default merge directory
// (CONTRACT.md §6.1 source 4, config.DefaultSecretsDir) contains any *.json.
// When it does, it is an active source that would shadow a lower-priority
// config-root write (source 5), so `secret set` must not write a file the
// loader will ignore.
//
// It is a package-level test seam: production never reassigns it; tests swap it
// to exercise the shadow path without control of /run/secrets (restoring it in
// t.Cleanup).
var defaultSecretsDirPopulated = func() bool {
	entries, err := os.ReadDir(config.DefaultSecretsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			return true
		}
	}
	return false
}

// generateClientToken returns a fresh random client token.
func generateClientToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "nk-" + hex.EncodeToString(buf), nil
}

// setSecretValue reads the target secrets file (empty when absent), sets the
// value (provider_keys[name] for kind "provider", client_token otherwise), and
// writes it back atomically with mode 0600. Unrelated keys are preserved.
func setSecretValue(path, kind, name, value string) error {
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if decodeErr := json.Unmarshal(data, &doc); decodeErr != nil {
			return fmt.Errorf("parse %s: %w", path, decodeErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if kind == "provider" {
		keys, _ := doc["provider_keys"].(map[string]any)
		if keys == nil {
			keys = map[string]any{}
		}
		keys[name] = value
		doc["provider_keys"] = keys
	} else {
		doc["client_token"] = value
	}

	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode secrets: %w", err)
	}
	encoded = append(encoded, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create secrets directory: %w", err)
	}
	return writeFileAtomic(path, encoded, 0o600)
}
