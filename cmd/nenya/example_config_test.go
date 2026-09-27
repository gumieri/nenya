package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nenya/config"
)

func TestHandleExampleConfig_PrintsLoadableConfig(t *testing.T) {
	var buf bytes.Buffer
	handled, err := handleExampleConfig(&buf, io.Discard, []string{"example-config"})
	if err != nil {
		t.Fatalf("handleExampleConfig: %v", err)
	}
	if !handled {
		t.Fatal("expected example-config to be handled")
	}
	if buf.Len() == 0 {
		t.Fatal("example-config printed nothing")
	}

	// The example must match the current schema: a strict decode rejects
	// unknown fields, and the full loader exercises comment stripping and
	// defaults (CONTRACT.md §4.4).
	dec := json.NewDecoder(bytes.NewReader(config.StripComments(buf.Bytes())))
	dec.DisallowUnknownFields()
	var probe config.Config
	if err := dec.Decode(&probe); err != nil {
		t.Fatalf("example config does not match the config schema: %v", err)
	}

	path := filepath.Join(t.TempDir(), "example.json")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write example: %v", err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("example config does not load: %v", err)
	}
}

func TestHandleExampleConfig_UnknownFlagFailsClosed(t *testing.T) {
	var errBuf bytes.Buffer
	handled, err := handleExampleConfig(io.Discard, &errBuf, []string{"example-config", "--nope"})
	if !handled {
		t.Fatal("expected example-config to handle the invocation")
	}
	if !errors.Is(err, errUsage) {
		t.Fatalf("expected errUsage, got %v", err)
	}
}

func TestHandleExampleConfig_Help(t *testing.T) {
	handled, err := handleExampleConfig(io.Discard, io.Discard, []string{"example-config", "-h"})
	if !handled || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got handled=%v err=%v", handled, err)
	}
}

func TestHandleExampleConfig_Passthrough(t *testing.T) {
	handled, err := handleExampleConfig(io.Discard, io.Discard, []string{"serve"})
	if handled || err != nil {
		t.Errorf("expected passthrough, got handled=%v err=%v", handled, err)
	}
}
