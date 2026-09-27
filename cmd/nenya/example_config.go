package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/nenya/examples"
)

// handleExampleConfig implements `nenya example-config` (CONTRACT.md §4.4): it
// prints the canonical example configuration so consumers never embed a copy.
// The printed document is the packaged examples/example.config.json, which the
// tests verify loads under the current config schema. It reports whether it
// handled the invocation.
func handleExampleConfig(w, errW io.Writer, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "example-config" {
		return false, nil
	}

	fs := flag.NewFlagSet("example-config", flag.ContinueOnError)
	fs.SetOutput(errW)
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, flag.ErrHelp
		}
		return true, errUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(errW, "unexpected argument: %s\n", fs.Arg(0))
		return true, errUsage
	}

	if _, err := w.Write(examples.ConfigJSON); err != nil {
		return true, fmt.Errorf("write example config: %w", err)
	}
	return true, nil
}
