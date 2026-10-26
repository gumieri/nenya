package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/nenya/config"
)

// TestSystemOneRetryResendsFullBody pins AUD-21 (NENYA-243): a retried
// System One decision call must carry the FULL request body on every
// attempt. Pre-fix the closure re-Dod the same *http.Request, whose body
// the first attempt had consumed — the retry arrived with an empty body and
// the provider billed a second decision call that carried no questions.
func TestSystemOneRetryResendsFullBody(t *testing.T) {
	type received struct {
		body string
	}
	var mu sync.Mutex
	var calls []received

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, received{body: string(bodyBytes)})
		mu.Unlock()
		if len(calls) == 1 {
			// First attempt fails 5xx: forces the retry path.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision": "answer",
			"usage":    map[string]any{"input_tokens": 7, "output_tokens": 3},
		})
	}))
	defer upstream.Close()

	gw := newTestGateway(&config.Config{}, map[string]*config.Provider{
		"prov": {Name: "prov", APIKey: "test-key"},
	})
	gw.Client = upstream.Client()
	p := &Proxy{}
	p.StoreGateway(gw)

	body := `{"state":{"thread":"t1"},"questions":[{"id":"q1","prompt":"decide"}],"model":"prov/system-one"}`
	w := httptest.NewRecorder()
	p.forwardSystemOneRequest(gw, w, context.Background(), upstream.URL+"/v1/systemone/decide",
		[]byte(body), "prov", "system-one", http.Header{}, 3, "test-key")

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected 2 upstream attempts (1 failure + 1 success), got %d", len(calls))
	}
	if w.Code != http.StatusOK {
		t.Fatalf("final status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	for i, c := range calls {
		if c.body != body {
			t.Fatalf("attempt %d received a %d-byte body, want the full %d-byte request (empty/truncated = the consumed-reader bug)\nsent: %q\ngot:  %q",
				i+1, len(c.body), len(body), body, c.body)
		}
	}
}
