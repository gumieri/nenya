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
	if len(args) >= 2 && isHelpArg(args[1]) {
		_, _ = fmt.Fprintln(errW, secretUsage)
		return true, flag.ErrHelp
	}
	_, _ = fmt.Fprintln(errW, secretUsage)
	return true, errUsage
}

// isHelpArg reports whether arg is one of the conventional help flags
// (CONTRACT.md §3.4: -h/--help prints usage and exits 0).
func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help"
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

	if setErr := applySecretSet(errW, target, *clientToken, *provider, fs.Args()); setErr != nil {
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
func applySecretSet(errW io.Writer, target string, clientToken bool, provider string, rest []string) error {
	switch {
	case clientToken:
		return applyClientToken(errW, target, rest)
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

// minClientTokenLen is the minimum length accepted for an explicit client
// token, matching ApiKey.Validate's floor for API keys. The generated token is
// far longer ("nk-" + 32 bytes hex).
const minClientTokenLen = 16

// applyClientToken sets client_token from rest[0], or generates one when no
// value is given. A malformed invocation returns errUsage; an explicit token
// shorter than minClientTokenLen is reported and returns errUsage, so main maps
// the malformed value to exit 2 (CONTRACT.md §3.4).
func applyClientToken(errW io.Writer, target string, rest []string) error {
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
	if len(token) < minClientTokenLen {
		_, _ = fmt.Fprintf(errW, "client token must be at least %d characters\n", minClientTokenLen)
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

// secretsFileTarget is a resolved single-file secrets target: the file to write
// and, when it lives in a merge directory, that directory (whose other *.json
// files can override keys by sort order, CONTRACT.md §6.2).
type secretsFileTarget struct {
	path     string
	mergeDir string
	// configRoot is true when path comes from the directory-mode config-root
	// branch (source 5), which is a single file unless the config root itself
	// is the default merge directory.
	configRoot bool
}

// nominalSecretsFileTarget returns the preferred single-file secrets target for
// the deployment, before any fail-closed checks (CONTRACT.md §4.7):
//   - an existing regular file named by NENYA_SECRETS_DIR is the target itself;
//   - else <NENYA_SECRETS_DIR>/secrets.json when the env var is set;
//   - else <config-root>/secrets.json in directory mode (CONTRACT.md §6.1
//     source 5 — a single file, not a merge directory);
//   - else /run/secrets/nenya/secrets.json in file mode (a merge directory).
func nominalSecretsFileTarget(paths configPaths) secretsFileTarget {
	if dir := secretsEnvDir(); dir != "" {
		if info, err := os.Stat(dir); err == nil && !info.IsDir() {
			return secretsFileTarget{path: dir}
		}
		return secretsFileTarget{path: filepath.Join(dir, "secrets.json"), mergeDir: dir}
	}
	if paths.file == "" {
		return secretsFileTarget{path: filepath.Join(config.CleanAbs(paths.dir), "secrets.json"), configRoot: true}
	}
	return secretsFileTarget{path: filepath.Join(config.DefaultSecretsDir, "secrets.json"), mergeDir: config.DefaultSecretsDir}
}

// secretTargetPath returns the secrets file `secret set` writes. Because a
// higher-priority source would shadow the write (CONTRACT.md §6.1), every
// branch fails closed rather than writing a file the loader will not read:
// systemd credentials (sources 1-2), a source-3/4 merge directory whose other
// files would override the target (§6.2 last-wins), and a populated default
// merge directory shadowing a config-root write (source 5).
func secretTargetPath(paths configPaths) (string, error) {
	return secretTargetPathWith(paths, defaultSecretsDirPopulated)
}

// secretTargetPathWith is secretTargetPath with the default-directory probe
// injected, so tests can exercise the shadow path without mutating a global or
// controlling /run/secrets.
func secretTargetPathWith(paths configPaths, defaultDirPopulated func() (bool, error)) (string, error) {
	if err := activeCredentialSource(); err != nil {
		return "", err
	}

	target := nominalSecretsFileTarget(paths)
	// A target in a merge directory — a source-3 env directory, file mode's
	// source-4 default, or a config root that *is* the default merge dir — is
	// subject to §6.2 last-wins, so a later-sorting sibling may override it.
	mergeDir := target.mergeDir
	// A source-5 target is a single file, but when the config root *is* the
	// default merge directory, the loader's source-4 directory merge applies,
	// so siblings there can still shadow it. An env-named single file is never
	// promoted: the loader reads it directly regardless of siblings.
	if mergeDir == "" && target.configRoot && sameDir(filepath.Dir(target.path), config.DefaultSecretsDir) {
		mergeDir = config.DefaultSecretsDir
	}
	if mergeDir != "" {
		shadowed, err := laterSiblingShadows(mergeDir, filepath.Base(target.path))
		if err != nil {
			return "", err
		}
		if shadowed {
			return "", fmt.Errorf("another *.json in %s sorts after %s and may shadow the written value; remove it or set NENYA_SECRETS_DIR to a dedicated directory", mergeDir, filepath.Base(target.path))
		}
	}
	// Source 5 is a single file, so only a populated default merge directory
	// can shadow it.
	if secretsEnvDir() == "" && paths.file == "" && !sameDir(filepath.Dir(target.path), config.DefaultSecretsDir) {
		populated, err := defaultDirPopulated()
		if err != nil {
			return "", err
		}
		if populated {
			return "", fmt.Errorf("active secrets source is %s (source 4) and may shadow %s; remove %s/*.json or set NENYA_SECRETS_DIR to a dedicated directory", config.DefaultSecretsDir, target.path, config.DefaultSecretsDir)
		}
	}
	return target.path, nil
}

// activeCredentialSource returns a fail-closed error when a systemd credential
// source (CONTRACT.md §6.1 sources 1-2) is active. The single-file credential
// is active only when it is a regular file the loader could read; a directory
// at that path is treated as absent by the loader and so is not active here.
func activeCredentialSource() error {
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	if credDir == "" {
		return nil
	}
	if info, err := os.Stat(filepath.Join(credDir, "secrets")); err == nil && !info.IsDir() {
		return fmt.Errorf("active secrets source is the systemd credential %s/secrets; manage it via systemd instead of writing a file", credDir)
	}
	has, err := dirHasSecrets(filepath.Join(credDir, "secrets.d"))
	if err != nil {
		return err
	}
	if has {
		return fmt.Errorf("active secrets source is the systemd credential directory %s/secrets.d; manage it via systemd instead of writing a file", credDir)
	}
	return nil
}

// dirHasSecrets reports whether dir holds at least one secrets document: a
// non-directory *.json other than config.json, which loadSecretsFromDir skips.
// A missing directory is not an error; any other read failure is, so an
// unreadable directory cannot silently disable a fail-closed guard.
func dirHasSecrets(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read secrets directory %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" && e.Name() != "config.json" {
			return true, nil
		}
	}
	return false, nil
}

// laterSiblingShadows reports whether dir holds a *.json (other than
// config.json) whose name sorts after name, so the §6.2 last-wins merge would
// let it override the target's keys.
func laterSiblingShadows(dir, name string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read secrets directory %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || e.Name() == "config.json" {
			continue
		}
		if e.Name() > name {
			return true, nil
		}
	}
	return false, nil
}

// defaultSecretsDirPopulated reports whether the default merge directory
// (CONTRACT.md §6.1 source 4) holds a secrets document. When it does, it may
// shadow a lower-priority config-root write (source 5). An unreadable directory
// is an error, not "empty", so a fail-closed guard is never silently disabled.
func defaultSecretsDirPopulated() (bool, error) {
	return dirHasSecrets(config.DefaultSecretsDir)
}

// sameDir reports whether a and b name the same directory, tolerating symlinks
// and path syntax when both exist.
func sameDir(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// ensureClientToken sets a generated client_token on doc when it has none.
// client_token is required (CONTRACT.md §6.2), so a provider-key write to a
// document without one would make the whole secrets source unloadable.
func ensureClientToken(doc map[string]any) error {
	if tok, _ := doc["client_token"].(string); tok != "" {
		return nil
	}
	generated, err := generateClientToken()
	if err != nil {
		return fmt.Errorf("generate client token: %w", err)
	}
	doc["client_token"] = generated
	return nil
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
//
// This is the single writer CONTRACT.md §4.7 describes: callers must serialize
// concurrent invocations against the same file. The read-modify-write is not
// locked, so two simultaneous `secret set` calls for different keys can lose
// one update; the atomic rename only makes each individual write torn-free.
func setSecretValue(path, kind, name, value string) error {
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if decodeErr := json.Unmarshal(data, &doc); decodeErr != nil {
			return fmt.Errorf("parse %s: %w", path, decodeErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if kind == "provider" {
		keys, _ := doc["provider_keys"].(map[string]any)
		if keys == nil {
			keys = map[string]any{}
		}
		keys[name] = value
		doc["provider_keys"] = keys
		if err := ensureClientToken(doc); err != nil {
			return err
		}
	} else {
		doc["client_token"] = value
	}

	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode secrets: %w", err)
	}
	encoded = append(encoded, '\n')

	// 0755 for the parent: in directory mode the target's parent is the config
	// root (CONTRACT.md §5.4 conventions), so it must not be narrowed to 0700.
	// The secrets file itself is 0600, which is the security-relevant mode.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create secrets directory: %w", err)
	}
	return writeFileAtomic(path, encoded, 0o600)
}
