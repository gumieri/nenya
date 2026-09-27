package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/version"
)

// describeVersion is the version block inside `describe --json` (CONTRACT.md
// §4.3, Appendix A.3).
type describeVersion struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// describeSecrets reports which secrets source won and which were searched, in
// priority order.
type describeSecrets struct {
	ActiveSource string   `json:"active_source"`
	Searched     []string `json:"searched"`
}

// describeModel is one catalog entry.
type describeModel struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ContextWindow int    `json:"context_window"`
	MaxOutput     int    `json:"max_output"`
}

// describeProviders reports the usable providers and the model catalog.
type describeProviders struct {
	Configured []string        `json:"configured"`
	Catalog    []describeModel `json:"catalog"`
}

// describeJSON is the machine-readable effective state (CONTRACT.md §4.3,
// Appendix A.3).
type describeJSON struct {
	ContractVersion int                 `json:"contract_version"`
	Version         describeVersion     `json:"version"`
	Paths           pathsJSON           `json:"paths"`
	Secrets         describeSecrets     `json:"secrets"`
	Config          *config.Config      `json:"config"`
	Providers       describeProviders   `json:"providers"`
	Diagnostics     []config.Diagnostic `json:"diagnostics"`
}

// handleDescribe implements `nenya describe [--json]` (CONTRACT.md §4.3): the
// single source of truth for the effective configuration, resolved paths, the
// secrets source that won, the model catalog, and diagnostics. It reports
// whether it handled the invocation.
func handleDescribe(w, errW io.Writer, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "describe" {
		return false, nil
	}

	fs := flag.NewFlagSet("describe", flag.ContinueOnError)
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

	desc, err := buildDescription(effectiveConfigPaths(*configDir, *configFile))
	if err != nil {
		return true, err
	}

	if !*jsonOut {
		printDescription(w, desc)
		return true, nil
	}

	encoded, err := json.Marshal(desc)
	if err != nil {
		return true, fmt.Errorf("encode describe: %w", err)
	}
	_, err = fmt.Fprintln(w, string(encoded))
	return true, err
}

// buildDescription loads the effective configuration and resolves secrets
// without failing when secrets are absent (describe is a diagnostic surface).
func buildDescription(paths configPaths) (describeJSON, error) {
	var (
		cfg   *config.Config
		diags []config.Diagnostic
		err   error
	)
	if paths.file != "" {
		cfg, diags, err = config.LoadWithDiagnostics(paths.file)
	} else {
		cfg, diags, err = config.LoadFromDirWithDiagnostics(paths.dir)
	}
	notFound := errors.Is(err, config.ErrConfigNotFound)
	if err != nil && !notFound {
		return describeJSON{}, fmt.Errorf("load config: %w", err)
	}
	if notFound {
		// `describe` is a diagnostic surface (CONTRACT.md §4.3): a missing
		// config is reported as a diagnostic and the effective config falls
		// back to defaults, so consumers can probe before bootstrap instead of
		// having to distinguish "command absent" from "no config yet".
		cfg = &config.Config{}
		if defaultErr := config.ApplyDefaults(cfg); defaultErr != nil {
			return describeJSON{}, fmt.Errorf("apply defaults: %w", defaultErr)
		}
		// The loader's message already names the paths tried; use it directly
		// rather than nesting it in another prefix.
		diags = append(diags, config.Diagnostic{
			Level:   "warn",
			Code:    "config_not_found",
			Message: strings.TrimPrefix(err.Error(), config.ErrConfigNotFound.Error()+": "),
			Source:  configSource(paths),
		})
	}

	res, secretsErr := config.ResolveSecrets(secretsConfigRoot(paths))
	diagnostics := make([]config.Diagnostic, 0, len(diags)+1)
	diagnostics = append(diagnostics, diags...)
	switch {
	case secretsErr != nil:
		// Name the candidate whose read/validation failed when the resolver
		// reported one; fall back to the winning source (a validation failure
		// after a source was read).
		source := res.FailedSource
		if source == "" {
			source = res.ActiveSource
		}
		diagnostics = append(diagnostics, config.Diagnostic{
			Level:   "error",
			Code:    "secrets_invalid",
			Message: secretsErr.Error(),
			Source:  source,
		})
	case res.Secrets == nil:
		diagnostics = append(diagnostics, config.Diagnostic{
			Level:   "warn",
			Code:    "secrets_not_found",
			Message: "no secrets found; providers without credentials are omitted",
		})
	}

	return describeJSON{
		ContractVersion: version.ContractVersion,
		Version: describeVersion{
			Version:   version.Version,
			Commit:    version.Commit,
			BuildTime: version.BuildTime,
		},
		Paths:   resolvePaths(paths),
		Secrets: describeSecrets{ActiveSource: res.ActiveSource, Searched: res.Searched},
		Config:  cfg,
		Providers: describeProviders{
			Configured: configuredProviders(cfg, res.Secrets),
			Catalog:    describeCatalog(cfg),
		},
		Diagnostics: diagnostics,
	}, nil
}

// configSource names the config location a load was attempted from, for the
// config_not_found diagnostic: the selected single file in file mode, else the
// config root directory.
func configSource(paths configPaths) string {
	if paths.file != "" {
		return absOrSelf(paths.file)
	}
	return absOrSelf(paths.dir)
}

// configuredProviders returns the sorted names of providers that can serve
// requests: those with a resolved key or an auth-free style.
func configuredProviders(cfg *config.Config, secrets *config.SecretsConfig) []string {
	configured := []string{}
	for name, pc := range cfg.Providers {
		key := ""
		if secrets != nil {
			key = secrets.ProviderKeys[name]
		}
		if key != "" || pc.AuthStyle == config.AuthStyleNone {
			configured = append(configured, name)
		}
	}
	sort.Strings(configured)
	return configured
}

// describeCatalog returns the static model catalog merged with config
// overrides. It performs no network discovery, so `describe` is deterministic
// and works offline.
func describeCatalog(cfg *config.Config) []describeModel {
	base := discovery.NewModelCatalog()
	discovery.ApplyProviderNonChatToCatalog(base, cfg.Providers)
	merged := discovery.MergeCatalog(base, cfg)

	models := merged.AllModels()
	out := make([]describeModel, 0, len(models))
	for _, m := range models {
		out = append(out, describeModel{
			Provider:      m.Provider,
			Model:         m.ID,
			ContextWindow: m.MaxContext,
			MaxOutput:     m.MaxOutput,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// printDescription writes a short human-readable summary of the effective
// state (the machine surface is `describe --json`).
func printDescription(w io.Writer, d describeJSON) {
	_, _ = fmt.Fprintf(w, "contract_version=%d\nversion=%s\npaths.mode=%s\npaths.config_dir=%s\nsecrets.active_source=%s\nproviders.configured=%s\nproviders.catalog=%d\ndiagnostics=%d\n",
		d.ContractVersion, d.Version.Version, d.Paths.Mode, d.Paths.ConfigDir,
		d.Secrets.ActiveSource, strings.Join(d.Providers.Configured, ","),
		len(d.Providers.Catalog), len(d.Diagnostics))
}
