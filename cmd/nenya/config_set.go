package main

import (
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

// handleConfig implements `nenya config set <dotted.key> <value>`
// (CONTRACT.md §4.6): the config single writer. It reports whether it handled
// the invocation.
func handleConfig(w, errW io.Writer, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "config" {
		return false, nil
	}
	if len(args) < 2 || args[1] != "set" {
		_, _ = fmt.Fprintln(errW, "usage: nenya config set [--config <file> | --config-dir <dir>] <dotted.key> <value>")
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
	if err := setConfigKey(target, rest[0], parseConfigValue(rest[1])); err != nil {
		return true, err
	}
	_, err := fmt.Fprintln(w, target)
	return true, err
}

// configTargetPath returns the file `config set` writes: the selected single
// file in file mode, otherwise the managed drop-in under the config directory.
func configTargetPath(paths configPaths) string {
	if paths.file != "" {
		return absOrSelf(paths.file)
	}
	return filepath.Join(absOrSelf(paths.dir), "config.d", managedConfigDropIn)
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
// key, and writes it back atomically as pretty JSON with mode 0644. Unrelated
// keys are preserved.
func setConfigKey(path, dottedKey string, value any) error {
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if decodeErr := json.Unmarshal(config.StripComments(data), &doc); decodeErr != nil {
			return fmt.Errorf("parse %s: %w", path, decodeErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := setDottedKey(doc, dottedKey, value); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	encoded = append(encoded, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return writeFileAtomic(path, encoded, 0o644)
}

// setDottedKey sets value at the dotted key path, creating intermediate
// objects. It fails when an intermediate component is a non-object value.
func setDottedKey(doc map[string]any, dottedKey string, value any) error {
	parts := strings.Split(dottedKey, ".")
	cur := doc
	for i, part := range parts {
		if part == "" {
			return fmt.Errorf("invalid config key %q", dottedKey)
		}
		if i == len(parts)-1 {
			cur[part] = value
			return nil
		}
		next, ok := cur[part]
		if !ok {
			child := map[string]any{}
			cur[part] = child
			cur = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("config key %q is not an object", strings.Join(parts[:i+1], "."))
		}
		cur = child
	}
	return nil
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
	return nil
}
