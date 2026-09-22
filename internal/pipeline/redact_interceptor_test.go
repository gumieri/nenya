package pipeline

import (
	"bytes"
	"context"
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
	interceptor := NewRedactInterceptor(true, patterns, "[REDACTED]", metrics)

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

func TestRedactInterceptorContentArrays(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}
	metrics := infra.NewMetrics()
	interceptor := NewRedactInterceptor(true, patterns, "[REDACTED]", metrics)

	req := &InterceptRequest{
		Payload: map[string]any{
			"model": "demo-model",
		},
		Messages: []map[string]any{
			{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "key=AKIAIOSFODNN7EXAMPLE"},
					map[string]any{"type": "image_url", "url": "http://example.com/img.png"},
				},
			},
			{"role": "assistant", "content": "no secrets here"},
		},
	}

	res, err := interceptor.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Skip {
		t.Fatal("expected payload modification, got Skip")
	}

	part := req.Messages[0]["content"].([]any)[0].(map[string]any)
	if part["text"] != "key=[REDACTED]" {
		t.Errorf("expected text part redacted, got %v", part["text"])
	}
	unchanged := req.Messages[0]["content"].([]any)[1].(map[string]any)
	if unchanged["url"] != "http://example.com/img.png" {
		t.Errorf("expected non-text part untouched, got %v", unchanged["url"])
	}

	var buf bytes.Buffer
	metrics.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), "nenya_pipeline_redactions_total 1") {
		t.Fatalf("expected 1 recorded redaction from array part, got:\n%s", buf.String())
	}
}

func TestRedactInterceptorNilMetrics(t *testing.T) {
	patterns := []*regexp.Regexp{regexp.MustCompile(`AKIA[0-9A-Z]{16}`)}
	interceptor := NewRedactInterceptor(true, patterns, "[REDACTED]", nil)

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
