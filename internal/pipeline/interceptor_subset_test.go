package pipeline

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"testing"

	"github.com/nenya/config"
)

// TestDeterministicSecuritySubsetFilters verifies the subset's membership
// (marker interface, by construction), ordering, and inheritance of the
// parent chain's logger, metrics, and strict mode.
func TestDeterministicSecuritySubsetFilters(t *testing.T) {
	chain := NewInterceptorChain(slog.New(slog.NewTextHandler(io.Discard, nil)))
	chain.SetStrictMode(true)
	chain.Register(NewRedactInterceptor(true, []*regexp.Regexp{regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`)}, "redacted", nil))
	chain.Register(NewEntropyInterceptor(NewEntropyFilter(5.0, 20), "redacted", nil))
	injection, err := NewInjectionInterceptor(&config.InjectionConfig{Enabled: config.PtrTo(true)}, map[string]config.AgentConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("build injection interceptor: %v", err)
	}
	chain.Register(injection)
	chain.Register(&dagStubInterceptor{name: "tfidf", priority: 30})
	chain.Register(&dagStubInterceptor{name: "bouncer", priority: 50})

	sub := chain.DeterministicSecuritySubset()
	got := sub.List()
	if len(got) != 3 {
		t.Fatalf("expected 3 security interceptors in the subset, got %d: %v", len(got), got)
	}
	if got[0].Name() != "redact" || got[1].Name() != "injection" || got[2].Name() != "entropy" {
		t.Errorf("subset order = [%s, %s, %s], want priority order [redact, injection, entropy]", got[0].Name(), got[1].Name(), got[2].Name())
	}
	if !sub.strict {
		t.Error("subset must inherit the parent's strict mode")
	}
	if sub.logger == nil {
		t.Error("subset must inherit the parent's logger")
	}
	if sub.metrics != chain.metrics {
		t.Error("subset must inherit the parent's metrics")
	}
}

type dagStubInterceptor struct {
	name     string
	priority int
}

func (s *dagStubInterceptor) Name() string  { return s.name }
func (s *dagStubInterceptor) Priority() int { return s.priority }
func (s *dagStubInterceptor) CanHandle(context.Context, *InterceptRequest) bool {
	return false
}
func (s *dagStubInterceptor) Process(context.Context, *InterceptRequest) (*InterceptResult, error) {
	return nil, nil
}
