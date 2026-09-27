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
)

// handleSecret implements `nenya secret set` (CONTRACT.md §4.7): the secrets
// single writer. It reports whether it handled the invocation.
func handleSecret(w, errW io.Writer, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "secret" {
		return false, nil
	}
	if len(args) < 2 || args[1] != "set" {
		_, _ = fmt.Fprintln(errW, secretUsage)
		return true, errUsage
	}

	fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
	fs.SetOutput(errW)
	provider := fs.String("provider", "", "Provider name whose key is set")
	clientToken := fs.Bool("client-token", false, "Set (or generate) the client token")
	var configDir, configFile string
	fs.StringVar(&configDir, "config-dir", "", "Configuration directory whose secrets.json is written by default")
	fs.StringVar(&configFile, "config", "", "Single configuration file (file mode)")
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, flag.ErrHelp
		}
		return true, errUsage
	}

	if *clientToken && *provider != "" {
		_, _ = fmt.Fprintln(errW, "usage: --provider and --client-token are mutually exclusive")
		return true, errUsage
	}

	target, targetErr := secretTargetPath(effectiveConfigPaths(configDir, configFile))
	if targetErr != nil {
		return true, targetErr
	}

	if setErr := applySecretSet(target, *clientToken, *provider, fs.Args()); setErr != nil {
		if errors.Is(setErr, errUsage) {
			_, _ = fmt.Fprintln(errW, secretUsage)
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

// secretUsage is the `secret set` usage line.
const secretUsage = "usage: nenya secret set --provider <name> <api-key> | --client-token [<token>]"

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
//     wires via LoadCredential and the loader also searches (source 4), instead
//     of the /run/secrets/nenya default the unit shadows.
//  3. File mode — /run/secrets/nenya/secrets.json (no config root to prefer).
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
		return filepath.Join(absOrSelf(dir), "secrets.json"), nil
	}
	if paths.file == "" {
		return filepath.Join(absOrSelf(paths.dir), "secrets.json"), nil
	}
	return "/run/secrets/nenya/secrets.json", nil
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
