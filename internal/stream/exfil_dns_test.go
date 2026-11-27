package stream

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nenya/config"
)

// NENYA-354 (optional DNS-reachability check) pins: with dns_check
// enabled, a hostname that fails to resolve within the timeout is
// flagged `dns_unresolvable` (fabricated host); resolvable hosts pass;
// verdicts memoize per host; IP literals and the disabled state never
// resolve. The resolver is stubbed via the guard's resolveHost seam.

// dnsStubGuard builds a guard with the DNS check enabled and a scripted
// resolver: hosts in fails map to an error, everything else resolves.
func dnsStubGuard(t *testing.T, action string, fails map[string]error, calls *int) *ExfilGuard {
	t.Helper()
	enabled := true
	guard := NewExfilGuard(config.ExfilGuardConfig{
		Action: action,
		DNSCheck: &config.ExfilDNSCheckConfig{
			Enabled:   &enabled,
			TimeoutMs: 250,
		},
	}, nil)
	guard.resolveHost = func(_ context.Context, host string) ([]string, error) {
		*calls++
		if err, ok := fails[host]; ok {
			return nil, err
		}
		return []string{"93.184.216.34"}, nil
	}
	return guard
}

func TestExfilDNSUnresolvableFlagged(t *testing.T) {
	calls := 0
	guard := dnsStubGuard(t, config.ExfilActionStrip, map[string]error{
		"fabricated.example.net": errors.New("no such host"),
	}, &calls)
	content := "see [docs](https://fabricated.example.net/guide) and carry on"
	got, action, reason := guard.FilterContent(content)
	if action != ActionRedact {
		t.Fatalf("expected strip, got %v (reason %q)", action, reason)
	}
	if reason != exfilReasonDNSUnresolvable {
		t.Fatalf("reason = %q, want dns_unresolvable", reason)
	}
	if !strings.Contains(got, exfilStripPlaceholder) {
		t.Fatalf("unresolvable-host link not stripped: %q", got)
	}
}

func TestExfilDNSResolvablePasses(t *testing.T) {
	calls := 0
	guard := dnsStubGuard(t, config.ExfilActionLog, nil, &calls)
	content := "see [docs](https://real.example.com/guide) done"
	if _, action, reason := guard.FilterContent(content); action != ActionPass || reason != "" {
		t.Fatalf("resolvable host flagged: action %v reason %q", action, reason)
	}
	// Buffered path: full content integrity (the streaming emit holds
	// the whitespace-free tail per NENYA-205).
	got, action, reason := guard.InspectFull(content)
	if action != ActionPass || reason != "" || got != content {
		t.Fatalf("buffered pass drifted: action %v reason %q got %q", action, reason, got)
	}
}

func TestExfilDNSMemoizedPerHost(t *testing.T) {
	calls := 0
	guard := dnsStubGuard(t, config.ExfilActionLog, map[string]error{
		"fabricated.example.net": errors.New("no such host"),
	}, &calls)
	_, _, _ = guard.FilterContent("[a](https://fabricated.example.net/1) [b](https://fabricated.example.net/2) [c](https://fabricated.example.net/3)")
	if calls != 1 {
		t.Fatalf("resolver called %d times for one host, want 1", calls)
	}
}

func TestExfilDNSDisabledNeverResolves(t *testing.T) {
	calls := 0
	// Enabled stays nil => check off; the resolver stub must never run.
	guard := NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionStrip}, nil)
	guard.resolveHost = func(_ context.Context, _ string) ([]string, error) {
		calls++
		return nil, errors.New("must not be called")
	}
	content := "see [docs](https://fabricated.example.net/guide) done"
	got, action, reason := guard.InspectFull(content)
	if calls != 0 {
		t.Fatalf("disabled check resolved %d time(s)", calls)
	}
	if action != ActionPass || got != content || reason != "" {
		t.Fatalf("disabled check altered content: action %v reason %q", action, reason)
	}
}

func TestExfilDNSIPLiteralSkipsResolution(t *testing.T) {
	// Public IP literal with allow_ip_literals on: passes host checks, no name to resolve.
	calls := 0
	allow := true
	g := NewExfilGuard(config.ExfilGuardConfig{
		Action:          config.ExfilActionLog,
		AllowIPLiterals: &allow,
		DNSCheck: &config.ExfilDNSCheckConfig{
			Enabled: config.PtrTo(true),
		},
	}, nil)
	g.resolveHost = func(_ context.Context, _ string) ([]string, error) {
		calls++
		return nil, errors.New("must not be called")
	}
	if _, action, reason := g.FilterContent("see [x](https://93.184.216.34/a) done"); action != ActionPass || reason != "" {
		t.Fatalf("IP literal flagged: action %v reason %q", action, reason)
	}
	if calls != 0 {
		t.Fatalf("IP literal resolved %d time(s)", calls)
	}
}

func TestExfilDNSTimeoutCountsAsFailure(t *testing.T) {
	enabled := true
	guard := NewExfilGuard(config.ExfilGuardConfig{
		Action: config.ExfilActionLog,
		DNSCheck: &config.ExfilDNSCheckConfig{
			Enabled:   &enabled,
			TimeoutMs: 20,
		},
	}, nil)
	guard.resolveHost = func(ctx context.Context, _ string) ([]string, error) {
		// Block well past the 20ms budget; the bounded context must win.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, _, reason := guard.FilterContent("see [x](https://slow.example.com/a) done"); reason != exfilReasonDNSUnresolvable {
		t.Fatalf("reason = %q, want dns_unresolvable (timeout is failure)", reason)
	}
}

func TestExfilDNSOrderingCheapChecksFirst(t *testing.T) {
	// A URL that fails the QUERY check must not spend a resolution.
	calls := 0
	guard := dnsStubGuard(t, config.ExfilActionLog, nil, &calls)
	longQuery := "https://real.example.com/x?d=" + repeatA(400)
	guard.FilterContent("see " + longQuery + " done")
	if calls != 0 {
		t.Fatalf("query-violating URL spent %d resolution(s)", calls)
	}
}

func repeatA(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'A'
	}
	return string(b)
}

// TestExfilDNSInspectFullPath pins the buffered path: InspectFull carries
// the DNS verdict like the streaming window.
func TestExfilDNSInspectFullPath(t *testing.T) {
	calls := 0
	guard := dnsStubGuard(t, config.ExfilActionStrip, map[string]error{
		"fabricated.example.net": errors.New("no such host"),
	}, &calls)
	content := "before [x](https://fabricated.example.net/a) after"
	got, action, reason := guard.InspectFull(content)
	if action != ActionRedact || reason != exfilReasonDNSUnresolvable {
		t.Fatalf("action %v reason %q, want redact/dns_unresolvable", action, reason)
	}
	if !strings.Contains(got, exfilStripPlaceholder) {
		t.Fatalf("buffered strip missing placeholder: %q", got)
	}
}

func TestExfilDNSConfigTimeoutClamped(t *testing.T) {
	// Direct construction with an over-cap timeout clamps (config
	// boundary bypass cannot create a huge stall budget).
	guard := NewExfilGuard(config.ExfilGuardConfig{
		Action: config.ExfilActionLog,
		DNSCheck: &config.ExfilDNSCheckConfig{
			Enabled:   config.PtrTo(true),
			TimeoutMs: 999999,
		},
	}, nil)
	if guard.dnsTimeout != config.MaxExfilDNSTimeoutMs*time.Millisecond {
		t.Fatalf("timeout not clamped: %v", guard.dnsTimeout)
	}
}

func TestExfilDNSAllowlistedHostTrusted(t *testing.T) {
	// An allowlisted host is an explicit trust decision (often an
	// internal name unresolvable against a public resolver): the probe
	// must never run against it. (Non-allowlisted hosts with a non-empty
	// allowlist already die at host_not_allowed before the probe.)
	calls := 0
	guard := NewExfilGuard(config.ExfilGuardConfig{
		Action:       config.ExfilActionStrip,
		AllowedHosts: []string{"wiki.internal"},
		DNSCheck: &config.ExfilDNSCheckConfig{
			Enabled: config.PtrTo(true),
		},
	}, nil)
	guard.resolveHost = func(_ context.Context, host string) ([]string, error) {
		calls++
		return nil, errors.New("no such host")
	}
	content := "see [wiki](https://wiki.internal/x) done"
	got, action, reason := guard.InspectFull(content)
	if action != ActionPass || reason != "" || got != content {
		t.Fatalf("allowlisted host flagged: action %v reason %q", action, reason)
	}
	if calls != 0 {
		t.Fatalf("allowlisted host resolved %d time(s), want 0", calls)
	}
}

func TestExfilDNSMemoEvictsOnOverflow(t *testing.T) {
	// Past the memo budget the cache evicts instead of stopping: every
	// distinct host still costs exactly one resolution (the documented
	// stall bound holds for adversarial link tables).
	calls := 0
	guard := dnsStubGuard(t, config.ExfilActionLog, nil, &calls)
	var b strings.Builder
	const hosts = exfilMaxTrackedURLs + 50
	for i := 0; i < hosts; i++ {
		b.WriteString(" [x](https://h" + strconv.Itoa(i) + ".example.com/a)")
	}
	if _, _, reason := guard.InspectFull(b.String()); reason != "" {
		t.Fatalf("all hosts resolvable, got reason %q", reason)
	}
	if calls != hosts {
		t.Fatalf("resolver called %d time(s) for %d distinct hosts", calls, hosts)
	}
	if len(guard.dnsMemo) > exfilMaxTrackedURLs {
		t.Fatalf("memo grew past its budget: %d", len(guard.dnsMemo))
	}
}
