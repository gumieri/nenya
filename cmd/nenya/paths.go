package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nenya/config"
)

// pathsJSON is the machine-readable filesystem contract (CONTRACT.md §4.2,
// Appendix A.2).
type pathsJSON struct {
	Mode       string `json:"mode"`
	ConfigDir  string `json:"config_dir"`
	ConfigFile string `json:"config_file"`
	ConfigD    string `json:"config_d"`
	SecretsDir string `json:"secrets_dir"`
	// SecretsFile is the single secrets file the deployment uses, or null when
	// there is none to name (file mode).
	SecretsFile *string `json:"secrets_file"`
	SocketPath  *string `json:"socket_path"`
	Platform    string  `json:"platform"`
}

// handlePaths implements `nenya paths [--json]` (CONTRACT.md §4.2). It resolves
// the same config paths the server uses for the same flags and environment
// variables (effectiveConfigPaths) and never loads secrets, so it is safe to
// run before any config or secrets exist. It writes the result to w,
// usage/diagnostics to errW, and reports whether it handled the invocation.
func handlePaths(w, errW io.Writer, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "paths" {
		return false, nil
	}

	fs := flag.NewFlagSet("paths", flag.ContinueOnError)
	fs.SetOutput(errW)
	configDir, configFile := addConfigRootFlags(fs)
	jsonOut := fs.Bool("json", false, "Emit JSON")

	if err := parseCommandFlags(fs, args[1:]); err != nil {
		return true, err
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(errW, "unexpected argument: %s\n", fs.Arg(0))
		return true, errUsage
	}

	resolved := resolvePaths(effectiveConfigPaths(*configDir, *configFile))

	if *jsonOut {
		encoded, err := json.Marshal(resolved)
		if err != nil {
			return true, fmt.Errorf("encode paths: %w", err)
		}
		_, err = fmt.Fprintln(w, string(encoded))
		return true, err
	}

	_, err := fmt.Fprintf(w, "mode=%s\nconfig_dir=%s\nconfig_file=%s\nconfig_d=%s\nsecrets_dir=%s\nsecrets_file=%s\nsocket_path=%s\nplatform=%s\n",
		resolved.Mode, resolved.ConfigDir, resolved.ConfigFile, resolved.ConfigD,
		resolved.SecretsDir, nullableString(resolved.SecretsFile), nullableString(resolved.SocketPath), resolved.Platform)
	return true, err
}

// resolvePaths derives the filesystem contract from resolved config paths. In
// file mode the server reads only config_file; config_d is reported for
// information and is not read in that mode. secrets_dir is the configured drop
// location (NENYA_SECRETS_DIR or the default); systemd credentials may supply a
// higher-priority source that describe reports as the winner. socket_path is
// null because the shipped socket unit uses TCP socket activation, so there is
// no filesystem socket (CONTRACT.md §4.2, Appendix A.2).
func resolvePaths(paths configPaths) pathsJSON {
	res := pathsJSON{Platform: runtime.GOOS}
	if paths.file != "" {
		res.Mode = "file"
		res.ConfigFile = absOrSelf(paths.file)
		res.ConfigDir = filepath.Dir(res.ConfigFile)
	} else {
		res.Mode = "directory"
		res.ConfigDir = absOrSelf(paths.dir)
		res.ConfigFile = filepath.Join(res.ConfigDir, "config.json")
	}
	res.ConfigD = filepath.Join(res.ConfigDir, "config.d")
	res.SecretsDir = secretsDir()
	res.SecretsFile = secretsFile(paths)
	return res
}

// secretsFile returns the preferred single secrets file — the nominal default
// target of `secret set` (CONTRACT.md §4.7): <NENYA_SECRETS_DIR>/secrets.json
// when that env var is set, else <config-root>/secrets.json in directory mode,
// else null in file mode. In directory mode it is also the file the shipped
// unit wires via LoadCredential (CONTRACT.md §6.1 source 5); `secret set` fails
// closed when a higher-priority source would shadow it, so this is a nominal
// target, not proof of which source the loader will pick.
func secretsFile(paths configPaths) *string {
	if dir := secretsEnvDir(); dir != "" {
		f := filepath.Join(dir, "secrets.json")
		return &f
	}
	if paths.file == "" {
		f := filepath.Join(absOrSelf(paths.dir), "secrets.json")
		return &f
	}
	return nil
}

// secretsEnvDir returns NENYA_SECRETS_DIR as an absolute path, or "" when unset.
func secretsEnvDir() string {
	if dir := os.Getenv("NENYA_SECRETS_DIR"); dir != "" {
		return absOrSelf(dir)
	}
	return ""
}

// secretsDir returns the configured secrets merge directory (NENYA_SECRETS_DIR
// when set, otherwise the default) per CONTRACT.md §6.1.
func secretsDir() string {
	if dir := secretsEnvDir(); dir != "" {
		return dir
	}
	return config.DefaultSecretsDir
}

// absOrSelf returns p as an absolute, cleaned path, or p unchanged when it
// cannot be resolved.
func absOrSelf(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// nullableString renders a nullable path for human-readable output.
func nullableString(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}
