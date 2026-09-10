package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/internal/infra"
)

func TestRedactInterceptorRecordsMetrics(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),
	}
	metrics := infra.NewMetrics()
	interceptor := NewRedactInterceptor(true, patterns, "[REDACTED]", slog.New(slog.DiscardHandler), metrics)

	req := &InterceptRequest{
		Payload: map[string]any{
			"model": "demo-model",
		},
		Messages: []map[string]any{
			{"role": "user", "content": "key=AKIAIOSFODNN7EXAMPLE token=ghp_DEMO00000000000000000000000000000000 clean"},
		},
	}

	res, err := interceptor.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Skip {
		t.Fatal("expected payload modification, got Skip")
	}

	var buf bytes.Buffer
	metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), "nenya_pipeline_redactions_total 2") {
		t.Fatalf("expected 2 recorded redactions, got:\n%s", buf.String())
	}
}

func TestRedactInterceptorNilMetrics(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}
	interceptor := NewRedactInterceptor(true, patterns, "[REDACTED]", slog.New(slog.DiscardHandler), nil)

	req := &InterceptRequest{
		Payload: map[string]any{
			"model": "demo-model",
		},
		Messages: []map[string]any{
			{"role": "user", "content": "key=AKIAIOSFODNN7EXAMPLE"},
		},
	}

	if _, err := interceptor.Process(context.Background(), req); err != nil {
		t.Fatalf("Process with nil metrics: %v", err)
	}
}
