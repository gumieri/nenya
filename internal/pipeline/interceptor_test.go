package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/nenya/internal/infra"
)

// stubInterceptor is a configurable Interceptor for chain tests.
type stubInterceptor struct {
	name     string
	priority int
	strict   bool
	err      error
	reject   *RejectError
	handled  bool
}

func (s *stubInterceptor) Name() string                                          { return s.name }
func (s *stubInterceptor) Priority() int                                         { return s.priority }
func (s *stubInterceptor) CanHandle(_ context.Context, _ *InterceptRequest) bool { return true }
func (s *stubInterceptor) Process(_ context.Context, req *InterceptRequest) (*InterceptResult, error) {
	s.handled = true
	if s.reject != nil {
		return nil, s.reject
	}
	if s.err != nil {
		return nil, s.err
	}
	return &InterceptResult{Payload: req.Payload}, nil
}

// strictStub adds Strict() to the type set without constraining the flag.
type strictStub struct{ stubInterceptor }

func (s *strictStub) Strict() bool { return s.strict }

func newTestChain(t *testing.T, interceptors ...Interceptor) *InterceptorChain {
	t.Helper()
	chain := NewInterceptorChainWithMetrics(slog.New(slog.DiscardHandler), infra.NewMetrics())
	for _, i := range interceptors {
		chain.Register(i)
	}
	return chain
}

func baseRequest() *InterceptRequest {
	return &InterceptRequest{
		Payload:  map[string]any{"model": "test-agent"},
		Messages: []map[string]any{{"role": "user", "content": "hello"}},
	}
}

func TestExecuteStrictInterceptorAborts(t *testing.T) {
	boom := errors.New("boom")
	strictFail := &strictStub{stubInterceptor{name: "strict-fail", priority: 10, strict: true, err: boom}}
	downstream := &stubInterceptor{name: "downstream", priority: 20}
	chain := newTestChain(t, strictFail, downstream)

	req := baseRequest()
	_, err := chain.Execute(context.Background(), req)
	var strictErr *StrictError
	if !errors.As(err, &strictErr) {
		t.Fatalf("Execute() error = %v, want *StrictError", err)
	}
	if strictErr.Interceptor != "strict-fail" {
		t.Errorf("Interceptor = %q, want %q", strictErr.Interceptor, "strict-fail")
	}
	if strictErr.Kind != infra.ErrorKindInternal {
		t.Errorf("Kind = %q, want %q", strictErr.Kind, infra.ErrorKindInternal)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error chain lost the cause: %v", err)
	}
	if downstream.handled {
		t.Error("downstream interceptor ran after strict abort")
	}
}

func TestExecuteNonStrictInterceptorFallsThrough(t *testing.T) {
	// no Strict() in the type set: fail-open
	fail := &stubInterceptor{name: "fail-open", priority: 10, err: errors.New("boom")}
	downstream := &stubInterceptor{name: "downstream", priority: 20}
	chain := newTestChain(t, fail, downstream)

	req := baseRequest()
	if _, err := chain.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute() = %v, want fall-through", err)
	}
	if !downstream.handled {
		t.Error("downstream interceptor did not run after fail-open error")
	}
}

func TestExecuteStrictFalseFallsThrough(t *testing.T) {
	fail := &strictStub{stubInterceptor{name: "opted-out", priority: 10, strict: false, err: errors.New("boom")}}
	downstream := &stubInterceptor{name: "downstream", priority: 20}
	chain := newTestChain(t, fail, downstream)

	req := baseRequest()
	if _, err := chain.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute() = %v, want fall-through for Strict()==false", err)
	}
	if !downstream.handled {
		t.Error("downstream interceptor did not run")
	}
}

func TestExecuteRejectErrorAborts(t *testing.T) {
	rejecting := &stubInterceptor{name: "rejector", priority: 10, reject: &RejectError{
		Err:  errors.New("policy"),
		Kind: infra.ErrorKindInjection,
	}}
	downstream := &stubInterceptor{name: "downstream", priority: 20}
	chain := newTestChain(t, rejecting, downstream)

	req := baseRequest()
	_, err := chain.Execute(context.Background(), req)
	var reject *RejectError
	if !errors.As(err, &reject) {
		t.Fatalf("Execute() error = %v, want *RejectError", err)
	}
	if downstream.handled {
		t.Error("downstream interceptor ran after policy rejection")
	}
}

func TestExecuteChainStrictModeAbortsAnyError(t *testing.T) {
	fail := &stubInterceptor{name: "any", priority: 10, err: errors.New("boom")}
	chain := newTestChain(t, fail)
	chain.SetStrictMode(true)

	_, err := chain.Execute(context.Background(), baseRequest())
	var strictErr *StrictError
	if !errors.As(err, &strictErr) {
		t.Fatalf("Execute() error = %v, want *StrictError under chain strict mode", err)
	}
	if strictErr.Interceptor != "any" {
		t.Errorf("Interceptor = %q, want %q", strictErr.Interceptor, "any")
	}
}

func TestAgentNameFor(t *testing.T) {
	tests := []struct {
		name string
		req  *InterceptRequest
		want string
	}{
		{
			name: "AgentName preferred",
			req:  &InterceptRequest{Payload: map[string]any{"model": "payload-agent"}, AgentName: "resolved-agent"},
			want: "resolved-agent",
		},
		{
			name: "falls back to payload model",
			req:  &InterceptRequest{Payload: map[string]any{"model": "payload-agent"}},
			want: "payload-agent",
		},
		{
			name: "empty when neither set",
			req:  &InterceptRequest{Payload: map[string]any{}},
			want: "",
		},
		{
			name: "non-string model ignored",
			req:  &InterceptRequest{Payload: map[string]any{"model": 42}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AgentNameFor(tt.req); got != tt.want {
				t.Errorf("AgentNameFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInterceptorStrictHelper(t *testing.T) {
	if interceptorStrict(&stubInterceptor{name: "plain"}) {
		t.Error("plain interceptor must not be strict")
	}
	if !interceptorStrict(&strictStub{stubInterceptor{name: "strict", strict: true}}) {
		t.Error("Strict()==true must be reported strict")
	}
	if interceptorStrict(&strictStub{stubInterceptor{name: "opted-out", strict: false}}) {
		t.Error("Strict()==false must not be reported strict")
	}
}

func TestSecurityInterceptorsAreStrict(t *testing.T) {
	metrics := infra.NewMetrics()
	for _, tc := range []struct {
		name  string
		iface Interceptor
	}{
		{"redact", NewRedactInterceptor(true, nil, "label", metrics)},
		{"injection", mustInjectionInterceptor(t, metrics)},
		{"spotlight", NewSpotlightInterceptor(nil, nil, metrics)},
		{"entropy", NewEntropyInterceptor(nil, "label", metrics)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ok := tc.iface.(StrictInterceptor)
			if !ok || !s.Strict() {
				t.Errorf("%s interceptor must implement StrictInterceptor with Strict()==true", tc.name)
			}
		})
	}
}

func mustInjectionInterceptor(t *testing.T, metrics *infra.Metrics) Interceptor {
	t.Helper()
	injection, err := NewInjectionInterceptor(nil, nil, metrics, nil)
	if err != nil {
		t.Fatalf("NewInjectionInterceptor: %v", err)
	}
	return injection
}

func TestStrictErrorShape(t *testing.T) {
	cause := errors.New("cause")
	err := &StrictError{Err: cause, Interceptor: "x"}
	if err.Error() != "cause" {
		t.Errorf("Error() = %q, want cause text", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("Unwrap chain broken")
	}
	if !strings.Contains(err.Error(), "cause") {
		t.Error("message lost")
	}
}

func prometheusCounter(t *testing.T, m *infra.Metrics, metric, labels string) (string, bool) {
	t.Helper()
	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, metric+labels+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, metric+labels+" ")), true
		}
	}
	return "", false
}

func TestExecuteStrictAbortRecordsErrorMetric(t *testing.T) {
	metrics := infra.NewMetrics()
	chain := NewInterceptorChainWithMetrics(slog.New(slog.DiscardHandler), metrics)
	chain.Register(&strictStub{stubInterceptor{name: "strict-fail", priority: 10, strict: true, err: errors.New("boom")}})

	_, err := chain.Execute(context.Background(), baseRequest())
	var strictErr *StrictError
	if !errors.As(err, &strictErr) {
		t.Fatalf("Execute() error = %v, want *StrictError", err)
	}
	if v, ok := prometheusCounter(t, metrics, "nenya_interceptor_errors_total", `{name="strict-fail"}`); !ok || v != "1" {
		t.Errorf("interceptor error metric = %q (present=%v), want 1", v, ok)
	}
}

func TestExecuteRejectErrorSkipsErrorMetric(t *testing.T) {
	metrics := infra.NewMetrics()
	chain := NewInterceptorChainWithMetrics(slog.New(slog.DiscardHandler), metrics)
	chain.Register(&stubInterceptor{name: "rejector", priority: 10, reject: &RejectError{
		Err:  errors.New("policy"),
		Kind: infra.ErrorKindInjection,
	}})

	if _, err := chain.Execute(context.Background(), baseRequest()); err == nil {
		t.Fatal("expected rejection")
	}
	if _, ok := prometheusCounter(t, metrics, "nenya_interceptor_errors_total", `{name="rejector"}`); ok {
		t.Error("policy rejection must not record the interceptor error metric")
	}
}
