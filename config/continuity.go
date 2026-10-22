package config

import (
	"sort"
	"time"
)

// DefaultReloadDrainTimeout bounds a SIGHUP reload's wait for the replaced
// gateway's in-flight requests (NENYA-215) when
// governance.reload_drain_timeout_seconds is absent or non-positive.
var DefaultReloadDrainTimeout = 30 * time.Second

// MaxReloadDrainTimeoutSeconds caps governance.reload_drain_timeout_seconds:
// a reload must not wait out an unbounded stream on the old gateway.
const MaxReloadDrainTimeoutSeconds = 600

// EffectiveReloadDrainTimeout returns the reload drain bound: the configured
// seconds when positive, DefaultReloadDrainTimeout when absent, and the
// MaxReloadDrainTimeoutSeconds cap for anything larger. Nil-safe.
func (c *Config) EffectiveReloadDrainTimeout() time.Duration {
	n := 0
	if c != nil && c.Governance.ReloadDrainTimeoutSeconds != nil {
		n = *c.Governance.ReloadDrainTimeoutSeconds
	}
	if n <= 0 {
		return DefaultReloadDrainTimeout
	}
	if n > MaxReloadDrainTimeoutSeconds {
		return MaxReloadDrainTimeoutSeconds * time.Second
	}
	return time.Duration(n) * time.Second
}

// ServeContinuityIssues diffs two effective provider sets across a reload
// (NENYA-166) and returns the providers that would silently lose serving
// capacity: someone that resolved a serviceable credential before and would
// not after (absent from the new resolved set, or present but credential-less
// where its auth style requires one). stillConfigured is the provider-name set
// of the NEW config: a provider the operator removed from the configuration is
// a deliberate change and is allowed; one still configured but absent from its
// resolved set is a resolution failure (a compile error skips it silently) and
// must reject the reload. The reload caller must FAIL the whole reload on a
// non-empty result and keep the last-known-good configuration — the lenient
// swap-in semantics accept new entries but never drop one that was serving.
func ServeContinuityIssues(current, candidate map[string]*Provider, stillConfigured map[string]struct{}) []string {
	if len(current) == 0 {
		return nil
	}
	var lost []string
	for name, before := range current {
		if before == nil || before.APIKey == "" {
			continue // was not serving with a credential: nothing to lose
		}
		if _, kept := stillConfigured[name]; !kept {
			continue // removed from the configuration on purpose (NENYA-166)
		}
		after, stillThere := candidate[name]
		switch {
		case !stillThere || after == nil:
			// Still configured but failed to resolve (a compile error skips
			// the provider silently in ResolveProviders).
			lost = append(lost, name)
		case after.AuthStyle != AuthStyleNone && after.APIKey == "":
			// The NEW config still needs a credential and it is gone (the
			// secret reference now fails or the key was removed); a switch to
			// AuthStyleNone is a deliberate config change, not a loss.
			lost = append(lost, name)
		}
	}
	sort.Strings(lost)
	return lost
}
