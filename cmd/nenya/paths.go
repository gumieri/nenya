package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// pathsJSON is the machine-readable filesystem contract (CONTRACT.md §4.2,
// Appendix A.2).
type pathsJSON struct {
	Mode       string  `json:"mode"`
	ConfigDir  string  `json:"config_dir"`
	ConfigFile string  `json:"config_file"`
	ConfigD    string  `json:"config_d"`
	SecretsDir string  `json:"secrets_dir"`
	SocketPath *string `json:"socket_path"`
	Platform   string  `json:"platform"`
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
	var configDir, configFile string
	jsonOut := false
	fs.StringVar(&configDir, "config-dir", "", "Configuration directory (contains config.d/ or config.json)")
	fs.StringVar(&configFile, "config", "", "Single configuration file")
	fs.BoolVar(&jsonOut, "json", false, "Emit JSON")

	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, flag.ErrHelp
		}
		// flag.ContinueOnError already wrote the error and usage to errW.
		return true, errUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(errW, "unexpected argument: %s\n", fs.Arg(0))
		return true, errUsage
	}

	resolved := resolvePaths(effectiveConfigPaths(configDir, configFile))

	if jsonOut {
		encoded, err := json.Marshal(resolved)
		if err != nil {
			return true, fmt.Errorf("encode paths: %w", err)
		}
		_, err = fmt.Fprintln(w, string(encoded))
		return true, err
	}

	_, err := fmt.Fprintf(w, "mode=%s\nconfig_dir=%s\nconfig_file=%s\nconfig_d=%s\nsecrets_dir=%s\nsocket_path=%s\nplatform=%s\n",
		resolved.Mode, resolved.ConfigDir, resolved.ConfigFile, resolved.ConfigD,
		resolved.SecretsDir, socketPathString(resolved.SocketPath), resolved.Platform)
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
	return res
}

// secretsDir returns the configured secrets directory (NENYA_SECRETS_DIR when
// set, otherwise the default /run/secrets/nenya) per CONTRACT.md §6.1.
func secretsDir() string {
	if dir := os.Getenv("NENYA_SECRETS_DIR"); dir != "" {
		return absOrSelf(dir)
	}
	return "/run/secrets/nenya"
}

// absOrSelf returns p as an absolute, cleaned path, or p unchanged when it
// cannot be resolved.
func absOrSelf(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// socketPathString renders a nullable socket path for human-readable output.
func socketPathString(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}
