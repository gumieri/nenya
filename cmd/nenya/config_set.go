package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nenya/config"
)

// managedConfigDropIn is the drop-in `nenya config set` writes in directory
// mode. Its 99- prefix merges after the conventional 00-/20- drop-ins and the
// config.json base, so written values take effect (CONTRACT.md §4.6). A drop-in
// sorting after it still wins; the command prints the path so precedence can be
// verified with `nenya describe`.
const managedConfigDropIn = "99-nenya.json"

// configSetUsage is the `config set` usage line.
const configSetUsage = "usage: nenya config set [--config <file> | --config-dir <dir>] <dotted.key> <value>"

// handleConfig implements `nenya config set <dotted.key> <value>`
// (CONTRACT.md §4.6): the config single writer. It reports whether it handled
// the invocation.
func handleConfig(w, errW io.Writer, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "config" {
		return false, nil
	}
	if len(args) >= 2 && isHelpArg(args[1]) {
		_, _ = fmt.Fprintln(errW, configSetUsage)
		return true, flag.ErrHelp
	}
	if len(args) < 2 || args[1] != "set" {
		_, _ = fmt.Fprintln(errW, configSetUsage)
		return true, errUsage
	}

	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(errW)
	configDir, configFile := addConfigRootFlags(fs)
	if err := parseCommandFlags(fs, args[2:]); err != nil {
		return true, err
	}

	rest := fs.Args()
	if len(rest) != 2 {
		_, _ = fmt.Fprintln(errW, "usage: nenya config set [flags] <dotted.key> <value>")
		return true, errUsage
	}

	target := configTargetPath(effectiveConfigPaths(*configDir, *configFile))
	value := parseConfigValue(rest[1])
	// Credential-typed config values get the same boot-time strength check
	// the secrets writer applies, so `config set` cannot produce a config the
	// gateway refuses to start with (NENYA-192; the checks are the
	// placeholder/template/repeated-character/empty classes — no length
	// floor, matching ValidateConfigCredentialsStrength). The key is matched
	// case-insensitively (JSON decoding is) and only under the providers
	// subtree — other api_key-named leaves (e.g. mcp_servers headers) are not
	// credentials the strength pass covers.
	// NENYA-175: the schema hint runs BEFORE the credential strength gate, so
	// a typo'd key gets named even when the value is also rejected as weak.
	warnUnknownConfigKey(errW, rest[0])
	lowerKey := strings.ToLower(rest[0])
	if isCredentialKey(lowerKey) {
		credentialValueNotes(errW, rest[0], value)
		if s, isStr := value.(string); isStr {
			if err := config.ValidateProviderKeyStrength(s); err != nil {
				_, _ = fmt.Fprintf(errW, "%s rejected: %v\n", rest[0], err)
				return true, errUsage
			}
		}
	}
	if err := setConfigKey(target, rest[0], value); err != nil {
		return true, err
	}
	_, err := fmt.Fprintln(w, target)
	return true, err
}

// warnUnknownConfigKey warns on errW when dottedKey does not exist in the
// config schema (typo protection, NENYA-175): the key materializes as a
// single-path document and strict-decodes (DisallowUnknownFields) against
// config.Config — the same secondary pass the loader uses for its NENYA-19
// unknown-field warning. Map-typed subtrees (provider, agent, and MCP-server
// NAMES) accept arbitrary keys by construction, so only struct fields are
// checked; case-folded spellings of known fields decode fine (SERVER.-style
// folds match snake_case tags), while respelled keys ("MaxBodyBytes" vs
// "max_body_bytes") warn — exactly the keys the loader's diagnostic names.
// The warning is advisory and worded independently of the write's outcome
// (the credential strength gate or a write error may still refuse the
// operation): unknown keys are preserved on disk and ignored at load either
// way (lenient decoding always wins, NENYA-19).
func warnUnknownConfigKey(errW io.Writer, dottedKey string) {
	parts := strings.Split(dottedKey, ".")
	for _, part := range parts {
		if part == "" {
			return // malformed key: setDottedKey rejects it with its own error
		}
	}
	doc := map[string]any{}
	cur := doc
	for i, part := range parts {
		if i == len(parts)-1 {
			cur[part] = nil // the leaf value's type is irrelevant to the field check
			break
		}
		child := map[string]any{}
		cur[part] = child
		cur = child
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var probe config.Config
	if err := dec.Decode(&probe); err != nil {
		if name, ok := config.UnknownFieldName(err); ok {
			_, _ = fmt.Fprintf(errW,
				"warning: %q is not a known config key (possible typo); unknown keys are preserved and ignored at load\n", name)
		}
	}
}

// configTargetPath returns the file `config set` writes: the selected single
// file in file mode, otherwise the managed drop-in under the config directory.
func configTargetPath(paths configPaths) string {
	if paths.file != "" {
		return config.CleanAbs(paths.file)
	}
	return filepath.Join(config.CleanAbs(paths.dir), "config.d", managedConfigDropIn)
}

// parseConfigValue parses raw as JSON when it is valid JSON, otherwise treats
// it as a string (CONTRACT.md §4.6).
func parseConfigValue(raw string) any {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err == nil {
		return value
	}
	return raw
}

// setConfigKey reads the target document (empty when absent), sets the dotted
// key, and writes it back atomically with mode 0644 (or the existing file's
// mode). Unrelated keys are preserved.
//
// The document is edited by BYTE SPLICE, never re-marshaled (AUD-45 /
// NENYA-224): config files are JSONC, so re-encoding destroyed every operator
// comment and reordered keys (Go maps have no order, and json.MarshalIndent
// sorts them). The result is re-validated as JSON before it is written, so a
// splice bug can never leave an unparseable config behind.
func setConfigKey(path, dottedKey string, value any) error {
	var existing []byte
	if data, err := os.ReadFile(path); err == nil {
		existing = data
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	encoded, err := spliceConfigKey(existing, dottedKey, value)
	if err != nil {
		return err
	}
	// Fail closed on a malformed result: the file must stay loadable.
	var check map[string]any
	if err := json.Unmarshal(config.StripComments(encoded), &check); err != nil {
		return fmt.Errorf("internal: config edit produced invalid JSON for %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	// Preserve an existing file's permissions and default new files to 0644:
	// the shipped systemd unit runs a dynamic user, so forcing 0600 would make
	// a root-owned drop-in unreadable to the service (NENYA-183 review). An
	// operator who wants the file private (it may carry MCP header values)
	// chmods it once and the mode is kept.
	return writeFileAtomic(path, encoded, configFileMode(path))
}

// isCredentialKey reports whether a dotted key targets a provider credential
// leaf: providers.<name>.api_key (case-insensitive, JSON decoding is). The
// .credential suffix is kept for symmetry with the accounts[] shape, but
// Credential exists only on accounts[] entries, which dotted keys cannot
// reach — in practice only the api_key suffix fires on schema keys.
func isCredentialKey(lowerKey string) bool {
	return strings.HasPrefix(lowerKey, "providers.") &&
		(strings.HasSuffix(lowerKey, ".api_key") || strings.HasSuffix(lowerKey, ".credential"))
}

// credentialValueNotes prints the non-fatal notes a credential write owes
// the operator (AUD-74 / NENYA-300). Two facts, both easy to get wrong:
// env:/file: REFERENCE resolution runs over secrets.json provider_keys only,
// never the config file — a reference written here is a dead literal and
// belongs in provider_keys; but a LITERAL api_key in the config file IS a
// serving credential (the provider's legacy "default" account), so a JSON
// null removes a credential rather than a dead string.
func credentialValueNotes(errW io.Writer, key string, value any) {
	if s, isStr := value.(string); isStr && config.IsSecretReference(s) {
		_, _ = fmt.Fprintf(errW, "note: %s is written verbatim; env:/file: references are resolved only in secrets.json provider_keys, never in the config file — a live reference belongs there\n", key)
	}
	if value == nil {
		_, _ = fmt.Fprintf(errW, "note: %s = null removes this config-file credential; the secrets.json provider_keys credential is unaffected (if this leaf was the serving credential, serving falls back to it — and with no provider_keys credential the provider simply becomes credential-less)\n", key)
	}
}

// configFileMode returns the mode to use when rewriting a config file: the
// file's current mode when it exists, else 0644.
func configFileMode(path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return 0o644
	}
	return info.Mode().Perm()
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory and an atomic rename, with the given permissions.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".nenya-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	// Best-effort fsync of the directory so the rename is durable across a
	// crash. The rename has already committed the value, so a filesystem that
	// cannot sync a directory (or an open failure) must not fail the write.
	if dirFile, dirErr := os.Open(dir); dirErr == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
