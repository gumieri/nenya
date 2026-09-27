package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nenya/deploy"
)

// Default service-unit paths (CONTRACT.md §4.5).
const (
	defaultExecPath    = "/usr/bin/nenya"
	defaultConfigDir   = "/etc/nenya"
	defaultSecretsFile = "/etc/nenya/secrets.json"
)

var (
	execStartLineRe = regexp.MustCompile(`(?m)^ExecStart=.*$`)
	plistProgramRe  = plistStringRe("Program")
	plistConfigRe   = plistStringRe("NENYA_CONFIG_DIR")
	plistSecretsRe  = plistStringRe("NENYA_SECRETS_DIR")
)

// plistStringRe matches a plist <key>K</key><string>V</string> pair and captures
// the open/close markup so only the value is replaced.
func plistStringRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`(<key>` + regexp.QuoteMeta(key) + `</key>\s*<string>)[^<]*(</string>)`)
}

// handleServiceUnit implements `nenya service-unit` (CONTRACT.md §4.5): it
// prints the shipped unit for the requested init system with the caller's paths
// substituted, so consumers never parse unit files out of a release archive. It
// reports whether it handled the invocation.
func handleServiceUnit(w, errW io.Writer, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "service-unit" {
		return false, nil
	}

	fs := flag.NewFlagSet("service-unit", flag.ContinueOnError)
	fs.SetOutput(errW)
	initSystem := fs.String("init", "systemd", "Init system: systemd or launchd")
	execPath := fs.String("exec-path", defaultExecPath, "Absolute path to the installed binary")
	configDir := fs.String("config-dir", defaultConfigDir, "Config root the unit passes to the binary")
	secretsFile := fs.String("secrets-file", defaultSecretsFile, "Credential source wired into the unit")

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

	unit, err := renderServiceUnit(*initSystem, *execPath, *configDir, *secretsFile)
	if err != nil {
		_, _ = fmt.Fprintln(errW, err)
		return true, errUsage
	}
	if _, err := io.WriteString(w, unit); err != nil {
		return true, fmt.Errorf("write service unit: %w", err)
	}
	return true, nil
}

// renderServiceUnit returns the unit for initSystem with the supplied paths.
func renderServiceUnit(initSystem, execPath, configDir, secretsFile string) (string, error) {
	switch initSystem {
	case "systemd":
		return renderSystemdUnit(execPath, configDir, secretsFile), nil
	case "launchd":
		return renderLaunchdUnit(execPath, configDir, secretsFile), nil
	default:
		return "", fmt.Errorf("unknown init system %q (want systemd or launchd)", initSystem)
	}
}

// renderSystemdUnit substitutes the binary, secret, and (when non-default)
// config paths into the shipped unit. The default config root needs no
// --config-dir argument because it is the binary's built-in default.
func renderSystemdUnit(execPath, configDir, secretsFile string) string {
	unit := strings.ReplaceAll(deploy.SystemdService, defaultExecPath, execPath)
	unit = strings.ReplaceAll(unit, defaultSecretsFile, secretsFile)
	if configDir != defaultConfigDir {
		unit = strings.ReplaceAll(unit, defaultConfigDir, configDir)
		unit = execStartLineRe.ReplaceAllLiteralString(unit, "ExecStart="+execPath+" --config-dir "+configDir)
	}
	return unit
}

// renderLaunchdUnit substitutes the binary and environment paths into the
// shipped plist. launchd has no credential mechanism, so secrets are wired
// through NENYA_SECRETS_DIR (a path the loader accepts as a file or directory);
// the shipped unit points it at the secrets directory.
func renderLaunchdUnit(execPath, configDir, secretsFile string) string {
	unit := replacePlistValue(deploy.LaunchdPlist, plistProgramRe, execPath)
	unit = replacePlistValue(unit, plistConfigRe, configDir)
	return replacePlistValue(unit, plistSecretsRe, filepath.Dir(secretsFile))
}

// replacePlistValue replaces the value of every <key>K</key><string>V</string>
// pair matched by re. ReplaceAllStringFunc keeps the replacement literal, so a
// value containing '$' is not interpreted.
func replacePlistValue(plist string, re *regexp.Regexp, value string) string {
	return re.ReplaceAllStringFunc(plist, func(m string) string {
		sub := re.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		return sub[1] + value + sub[2]
	})
}
