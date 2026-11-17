package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/infra"
)

// Tests in this file pin NENYA-336: max_tpm must be enforced on non-chat
// endpoints, not just the chat path. Before the fix every non-chat handler
// called RateLimiter.Check with a hardcoded token count of 0, so the TPM
// bucket was never checked nor decremented — embeddings (the potentially
// huge-token path) ran RPM-only.

// TestHandleEmbeddings_ChargesTPMFromRealUsage pins the post-dispatch
// charge: the embeddings response's real usage must flow into the TPM
// bucket, so subsequent requests on the same provider bucket see the
// consumed budget. Before the fix this failed: the bucket stayed full and
// the follow-up Check passed.
func TestHandleEmbeddings_ChargesTPMFromRealUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":80,"completion_tokens":20,"total_tokens":100}}`))
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	gw := p.Gateway()
	// A deliberately small TPM budget: the response's real input total (80
	// prompt tokens) exceeds it, so the true-up must exhaust the bucket
	// (the completion leg is deliberately excluded from the charge).
	gw.RateLimiter = infra.NewRateLimiter(1000, 50)
	gw.Metrics.RateLimits = gw.RateLimiter.Snapshot // rebind the statsz snapshot after the override

	body := strings.NewReader(`{"model":"deepseek-v4-flash","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	p.handleEmbeddings(gw, rec, req, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The bucket must reflect the real usage: a follow-up request on the
	// same provider bucket has no TPM budget left.
	if gw.RateLimiter.Check("deepseek", upstream.URL+"/v1/chat/completions", 10) {
		t.Fatal("TPM bucket was not charged from the embeddings response usage: a follow-up request still passed with the budget exhausted")
	}

	// The input metric leg must carry the model label (not the bounded
	// unknown placeholder) so the client − input derivation stays possible.
	// Line-scoped on purpose: the buffer also holds client/output legs with
	// the same model label — a whole-buffer Contains would false-pass if
	// only the input leg (embeddings.go, the site this pins) regressed.
	var prom bytes.Buffer
	gw.Metrics.WritePrometheus(&prom)
	inputLegFound := false
	for _, line := range strings.Split(prom.String(), "\n") {
		if !strings.Contains(line, "nenya_tokens_estimated_total") || !strings.Contains(line, `direction="input"`) {
			continue
		}
		inputLegFound = true
		if !strings.Contains(line, `model="deepseek-v4-flash"`) {
			t.Errorf("input-direction estimated-tokens line lost the model label: %s", line)
		}
		if strings.Contains(line, `model="_invalid_"`) {
			t.Errorf("input-direction estimated-tokens line collapsed the model label to _invalid_: %s", line)
		}
	}
	if !inputLegFound {
		t.Error("no input-direction nenya_tokens_estimated_total line found for the embeddings response")
	}
}

// TestHandleSystemOne_EnforcesTPMAtAdmission pins the admission-time
// enforcement: handleSystemOne counts the request body with the project
// tokenizer, so it must pass the REAL token count to the limiter — the
// second request whose cost exceeds the REMAINING TPM budget is rejected
// 429 at admission instead of sailing through on the RPM-only check.
// Before the fix this failed: the hardcoded 0 token count skipped the TPM
// dimension and both requests returned 200. (A single request larger than
// the whole per-minute budget is admitted by design — the limiter's
// tokenCount < maxTPM guard; this pin exercises the within-budget class.)
func TestHandleSystemOne_EnforcesTPMAtAdmission(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":[{"choice":{"verdict":"ok"}}]}`))
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	gw := p.Gateway()

	// A provider serving the System One decision endpoint, with the model
	// seeded into the catalog so ResolveProviders resolves through it.
	gw.Providers["test-provider"].FormatURLs = map[string]string{
		config.FormatKeySystemOne: upstream.URL + "/decision",
	}
	gw.ModelCatalog.Add(discovery.DiscoveredModel{ID: "test-model", Provider: "test-provider"})

	question := "Should this decision request be admitted under the token budget? "
	makeBody := func(id string) string {
		return `{"model":"test-model","questions":[{"id":"` + id + `","text":"` + strings.Repeat(question, 2) + `"}]}`
	}
	bodyA, bodyB := makeBody("q1"), makeBody("q2")

	// Self-calibrate: maxTPM sized so A is admitted with a remaining budget
	// at least half of B — B must then be rejected against the remainder.
	// The half-B deficit also gives refill slack: the bucket refill
	// between A and B credits maxTPM·gap/60s, which cannot flip B back to
	// admitted unless the test runner stalls long enough for the refill to
	// bridge the deficit — for countA=countB that threshold is ≈20s
	// (deficit·60/maxTPM), and the assertions below run immediately after
	// A, so the window is practically nil on any healthy runner.
	countA := gw.CountTokens(bodyA)
	countB := gw.CountTokens(bodyB)
	if countA <= 0 || countB <= 0 {
		t.Fatalf("tokenizer returned a non-positive count: A=%d B=%d", countA, countB)
	}
	if countB < 2 {
		t.Fatalf("need at least 2 tokens in body B for the deficit arithmetic, got %d", countB)
	}
	maxTPM := countA + countB - countB/2
	gw.RateLimiter = infra.NewRateLimiter(1000, maxTPM)
	gw.Metrics.RateLimits = gw.RateLimiter.Snapshot // rebind the statsz snapshot after the override

	reqA := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(bodyA))
	reqA.Header.Set("Authorization", "Bearer test-token")
	recA := httptest.NewRecorder()
	p.handleSystemOne(gw, recA, reqA, nil)
	if recA.Code != http.StatusOK {
		t.Fatalf("expected 200 for the first (within-budget) request, got %d: %s", recA.Code, recA.Body.String())
	}

	reqB := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(bodyB))
	reqB.Header.Set("Authorization", "Bearer test-token")
	recB := httptest.NewRecorder()
	p.handleSystemOne(gw, recB, reqB, nil)
	if recB.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for the second request (TPM remainder %d < cost %d), got %d: %s", maxTPM-countA, countB, recB.Code, recB.Body.String())
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("the TPM-rejected request must not reach upstream; got %d upstream hits, want 1", upstreamHits.Load())
	}
}
