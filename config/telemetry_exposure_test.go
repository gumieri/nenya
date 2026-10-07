package config

import (
	"log/slog"
	"testing"
)

// TestValidateTelemetryExposure covers the NENYA-131 warning: the
// telemetry_unauthenticated opt-out must warn on non-loopback listeners.
func TestValidateTelemetryExposure(t *testing.T) {
	cases := []struct {
		name       string
		listenAddr string
		unauth     bool
		wantWarn   bool
	}{
		{name: "opt-out on loopback", listenAddr: "127.0.0.1:8080", unauth: true, wantWarn: false},
		{name: "opt-out on localhost name", listenAddr: "localhost:8080", unauth: true, wantWarn: false},
		{name: "opt-out on wildcard", listenAddr: ":8080", unauth: true, wantWarn: true},
		{name: "opt-out on all-interfaces", listenAddr: "0.0.0.0:8080", unauth: true, wantWarn: true},
		{name: "opt-out on external address", listenAddr: "192.168.1.10:8080", unauth: true, wantWarn: true},
		{name: "default auth on wildcard", listenAddr: ":8080", unauth: false, wantWarn: false},
		{name: "opt-out on IPv6 loopback", listenAddr: "[::1]:8080", unauth: true, wantWarn: false},
		{name: "opt-out on malformed addr", listenAddr: "no-a-host:port", unauth: true, wantWarn: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Server.ListenAddr = tc.listenAddr
			cfg.Server.TelemetryUnauthenticated = tc.unauth

			var warnings []string
			logger := slog.New(slog.NewTextHandler(&testWarningSink{lines: &warnings}, nil))
			validateTelemetryExposure(cfg, logger)

			if tc.wantWarn && len(warnings) == 0 {
				t.Errorf("expected a warning for listen_addr %q with telemetry_unauthenticated, got none", tc.listenAddr)
			}
			if !tc.wantWarn && len(warnings) != 0 {
				t.Errorf("unexpected warning for listen_addr %q: %v", tc.listenAddr, warnings)
			}
		})
	}
}

// testWarningSink captures slog text output lines for assertion.
type testWarningSink struct {
	lines *[]string
}

func (w *testWarningSink) Write(p []byte) (int, error) {
	*w.lines = append(*w.lines, string(p))
	return len(p), nil
}
