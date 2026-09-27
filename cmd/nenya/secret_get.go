package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/nenya/config"
)

// secretGetUsage is the `secret get` usage line.
const secretGetUsage = "usage: nenya secret get [--config-dir <dir> | --config <file>] --client-token | --provider <name>"

// handleSecretGet implements `nenya secret get` (CONTRACT.md §4.8): the secrets
// single reader. It resolves the effective secrets source (same precedence as
// the server) and writes the requested value to w. The value is never logged,
// and a value is only ever written to stdout, so consumers never reimplement
// the §6.1 search order or the §6.2 merge.
func handleSecretGet(w, errW io.Writer, args []string) (bool, error) {
	fs := flag.NewFlagSet("secret get", flag.ContinueOnError)
	// A parse error can echo a flag value, and this command handles secrets, so
	// the flag package's output is discarded and usage is printed explicitly.
	fs.SetOutput(io.Discard)
	provider := fs.String("provider", "", "Provider name whose key is read")
	clientToken := fs.Bool("client-token", false, "Read the client token")
	configDir, configFile := addConfigRootFlags(fs)
	if err := parseCommandFlags(fs, args[2:]); err != nil {
		// The flag package's output is discarded, so print usage for both a
		// usage error and -h/--help (CONTRACT.md §3.4).
		_, _ = fmt.Fprintln(errW, secretGetUsage)
		return true, err
	}
	if fs.NArg() > 0 {
		// Do not echo the argument: `secret get --client-token <value>` is a
		// plausible mistake and would leak the value to stderr.
		_, _ = fmt.Fprintln(errW, secretGetUsage)
		return true, errUsage
	}
	// Exactly one selector is required.
	if *clientToken && *provider != "" {
		_, _ = fmt.Fprintln(errW, "usage: --provider and --client-token are mutually exclusive")
		return true, errUsage
	}
	if !*clientToken && *provider == "" {
		_, _ = fmt.Fprintln(errW, secretGetUsage)
		return true, errUsage
	}

	paths := effectiveConfigPaths(*configDir, *configFile)
	res, err := config.ResolveSecrets(secretsConfigRoot(paths))
	if err != nil {
		return true, fmt.Errorf("read secrets: %w", err)
	}
	if res.Secrets == nil {
		return true, fmt.Errorf("no secrets found (considered %s)", strings.Join(res.Searched, ", "))
	}

	if *clientToken {
		_, err = fmt.Fprintln(w, res.Secrets.ClientToken)
		return true, err
	}
	key, ok := res.Secrets.ProviderKeys[*provider]
	if !ok || key == "" {
		return true, fmt.Errorf("no key configured for provider %q", *provider)
	}
	_, err = fmt.Fprintln(w, key)
	return true, err
}
