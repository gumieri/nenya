package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/nenya/config"
)

// secretGetUsage is the `secret get` usage line.
const secretGetUsage = "usage: nenya secret get --client-token | --provider <name>"

// handleSecretGet implements `nenya secret get` (CONTRACT.md §4.7): the secrets
// single reader. It resolves the effective secrets source (same precedence as
// the server) and writes the requested value to w. The value is never logged,
// and a value is only ever written to stdout, so consumers never reimplement
// the §6.1 search order or the §6.2 merge.
func handleSecretGet(w, errW io.Writer, args []string) (bool, error) {
	fs := flag.NewFlagSet("secret get", flag.ContinueOnError)
	fs.SetOutput(errW)
	provider := fs.String("provider", "", "Provider name whose key is read")
	clientToken := fs.Bool("client-token", false, "Read the client token")
	var configDir, configFile string
	fs.StringVar(&configDir, "config-dir", "", "Configuration directory (directory mode)")
	fs.StringVar(&configFile, "config", "", "Single configuration file (file mode)")
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, flag.ErrHelp
		}
		return true, errUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(errW, "unexpected argument: %s\n", fs.Arg(0))
		return true, errUsage
	}
	// Exactly one selector is required.
	if *clientToken == (*provider != "") {
		_, _ = fmt.Fprintln(errW, secretGetUsage)
		return true, errUsage
	}

	paths := effectiveConfigPaths(configDir, configFile)
	res, err := config.ResolveSecrets(secretsConfigRoot(paths))
	if err != nil {
		return true, fmt.Errorf("read secrets: %w", err)
	}
	if res.Secrets == nil {
		return true, fmt.Errorf("no secrets found (checked %s)", strings.Join(res.Searched, ", "))
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
