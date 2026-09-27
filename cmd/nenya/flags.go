package main

import (
	"errors"
	"flag"
)

// addConfigRootFlags registers the shared config-root selection flags
// (--config-dir/--config) on fs and returns their destinations. Every
// inspection or writer subcommand resolves paths with the same flags and
// environment as the server (CONTRACT.md §3.3).
func addConfigRootFlags(fs *flag.FlagSet) (dir, file *string) {
	dir = fs.String("config-dir", "", "Configuration directory (contains config.d/ or config.json)")
	file = fs.String("config", "", "Single configuration file")
	return dir, file
}

// parseCommandFlags parses args and maps the flag package's error to the
// handler contract: help is propagated so main exits 0, and every other parse
// error becomes errUsage (which main maps to exit 2). The FlagSet's output must
// already be set by the caller; the flag package writes the error and usage
// there (handlers that must not echo argument values discard it).
func parseCommandFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return errUsage
	}
	return nil
}
