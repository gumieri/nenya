package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/testutil"
)

// NENYA-353 (tool-capability egress policy) pins: a sensitive request
// (the chain's redaction signals fired) carrying networked tool
// declarations is acted on per governance.tool_egress.action — log
// (default, inert), strip (declarations + naming tool_choice removed),
// block (403 exfil_blocked). Clean requests and the kill switch pass
// tools through untouched.

// newToolEgressGateway builds a gateway with the given tool-egress config.
func newToolEgressGateway(t *testing.T, te *config.ToolEgressConfig) *gateway.NenyaGateway {
	t.Helper()
	cfg := testutil.MinimalConfig()
	if te != nil {
		cfg.Governance.ToolEgress = te
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	return gateway.New(t.Context(), *cfg, secrets, slog.Default())
}

// toolDecl is a small helper building an OpenAI function declaration.
func toolDecl(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": "d"}}
}

// sensitivePayload returns a payload with one local and one networked
// declaration plus a forced tool_choice.
func sensitivePayload() map[string]any {
	return map[string]any{
		"model":       "demo-agent",
		"messages":    []any{map[string]any{"role": "user", "content": "x"}},
		"tools":       []any{toolDecl("local_lookup"), toolDecl("web_search")},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "web_search"}},
	}
}

// TestToolEgressBlockRejects pins the block action: a sensitive request
// with a networked declaration is rejected 403 error_kind=exfil_blocked.
func TestToolEgressBlockRejects(t *testing.T) {
	gw := newToolEgressGateway(t, &config.ToolEgressConfig{Action: "block"})
	p := &Proxy{}
	rec := httptest.NewRecorder()
	if _, ok := p.applyToolEgressPolicy(gw, rec, sensitivePayload(), "demo-agent", true); ok {
		t.Fatal("block action must stop the dispatch")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body struct {
		ErrorKind string `json:"error_kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.ErrorKind != "exfil_blocked" {
		t.Fatalf("error_kind = %q (err %v), want exfil_blocked", body.ErrorKind, err)
	}
}

// TestToolEgressBlockIgnoresCleanRequest pins that the policy never fires
// without a sensitivity signal: same tools, clean request → dispatch
// continues untouched.
func TestToolEgressBlockIgnoresCleanRequest(t *testing.T) {
	gw := newToolEgressGateway(t, &config.ToolEgressConfig{Action: "block"})
	p := &Proxy{}
	payload := sensitivePayload()
	rec := httptest.NewRecorder()
	if _, ok := p.applyToolEgressPolicy(gw, rec, payload, "demo-agent", false); !ok {
		t.Fatal("clean request must dispatch")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("clean request got a response written: %d", rec.Code)
	}
	if len(payload["tools"].([]any)) != 2 {
		t.Fatal("clean request tools must stay untouched")
	}
}

// TestToolEgressStrip pins the strip action: the networked declaration is
// removed, the local one stays, the naming tool_choice is dropped.
func TestToolEgressStrip(t *testing.T) {
	gw := newToolEgressGateway(t, &config.ToolEgressConfig{Action: "strip"})
	p := &Proxy{}
	payload := sensitivePayload()
	if _, ok := p.applyToolEgressPolicy(gw, httptest.NewRecorder(), payload, "demo-agent", true); !ok {
		t.Fatal("strip must continue the dispatch")
	}
	tools := payload["tools"].([]any)
	if len(tools) != 1 || declaredToolName(tools[0].(map[string]any)) != "local_lookup" {
		t.Fatalf("tools after strip = %+v, want only local_lookup", tools)
	}
	if _, has := payload["tool_choice"]; has {
		t.Fatal("tool_choice naming a stripped tool must be dropped")
	}
}

// TestToolEgressStripFlatNameShape pins the flat declaration shape
// (Anthropic/Responses-style {"name": ...}): it is classified and
// stripped like the OpenAI wrapper shape.
func TestToolEgressStripFlatNameShape(t *testing.T) {
	gw := newToolEgressGateway(t, &config.ToolEgressConfig{Action: "strip"})
	p := &Proxy{}
	payload := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"tools": []any{
			map[string]any{"name": "fetch_url", "description": "d"},
			map[string]any{"name": "db_query", "description": "d"},
		},
		"tool_choice": map[string]any{"type": "tool", "name": "fetch_url"},
	}
	if _, ok := p.applyToolEgressPolicy(gw, httptest.NewRecorder(), payload, "demo-agent", true); !ok {
		t.Fatal("strip must continue the dispatch")
	}
	tools := payload["tools"].([]any)
	if len(tools) != 1 || declaredToolName(tools[0].(map[string]any)) != "db_query" {
		t.Fatalf("tools after strip = %+v, want only db_query", tools)
	}
	if _, has := payload["tool_choice"]; has {
		t.Fatal("flat tool_choice naming a stripped tool must be dropped")
	}
}

// TestToolEgressStripAllToolsDeletesField pins that stripping the LAST
// declaration deletes payload["tools"] rather than leaving an empty array
// (a bare empty tools array is a provider 400 waiting to happen) and
// takes the orphaned tool_choice with it — the MCP injection sets
// tool_choice:"auto" on every successful injection, so the flagship
// fully-networked agent would otherwise dispatch choice-without-tools
// and take a guaranteed upstream 400.
func TestToolEgressStripAllToolsDeletesField(t *testing.T) {
	gw := newToolEgressGateway(t, &config.ToolEgressConfig{Action: "strip"})
	p := &Proxy{}
	payload := map[string]any{
		"messages":    []any{map[string]any{"role": "user", "content": "x"}},
		"tools":       []any{toolDecl("web_search")},
		"tool_choice": "auto",
	}
	if _, ok := p.applyToolEgressPolicy(gw, httptest.NewRecorder(), payload, "demo-agent", true); !ok {
		t.Fatal("strip must continue the dispatch")
	}
	if _, has := payload["tools"]; has {
		t.Fatal("fully stripped tool list must delete the field, not leave []")
	}
	if _, has := payload["tool_choice"]; has {
		t.Fatal("orphaned tool_choice must be deleted with the last declaration")
	}
}

// TestToolEgressStripKeepsStringToolChoice pins that string tool_choice
// values ("auto"/"none"/"required") survive a strip.
func TestToolEgressStripKeepsStringToolChoice(t *testing.T) {
	gw := newToolEgressGateway(t, &config.ToolEgressConfig{Action: "strip"})
	p := &Proxy{}
	payload := sensitivePayload()
	payload["tool_choice"] = "auto"
	if _, ok := p.applyToolEgressPolicy(gw, httptest.NewRecorder(), payload, "demo-agent", true); !ok {
		t.Fatal("strip must continue the dispatch")
	}
	if payload["tool_choice"] != "auto" {
		t.Fatalf("string tool_choice lost: %v", payload["tool_choice"])
	}
}

// TestToolEgressDefaultLogIsInert pins the default action: log fires but
// the payload is untouched — zero behavior change without operator opt-in.
func TestToolEgressDefaultLogIsInert(t *testing.T) {
	gw := newToolEgressGateway(t, nil)
	p := &Proxy{}
	payload := sensitivePayload()
	if _, ok := p.applyToolEgressPolicy(gw, httptest.NewRecorder(), payload, "demo-agent", true); !ok {
		t.Fatal("log must continue the dispatch")
	}
	if len(payload["tools"].([]any)) != 2 || payload["tool_choice"] == nil {
		t.Fatal("log action must not mutate the payload")
	}
}

// TestToolEgressKillSwitch pins enabled=false: no action, even block, is
// applied — the operator can restore the pre-NENYA-353 dispatch.
func TestToolEgressKillSwitch(t *testing.T) {
	off := false
	gw := newToolEgressGateway(t, &config.ToolEgressConfig{Enabled: &off, Action: "block"})
	p := &Proxy{}
	payload := sensitivePayload()
	if _, ok := p.applyToolEgressPolicy(gw, httptest.NewRecorder(), payload, "demo-agent", true); !ok {
		t.Fatal("kill switch must continue the dispatch")
	}
	if len(payload["tools"].([]any)) != 2 {
		t.Fatal("kill switch must leave tools untouched")
	}
}

// TestToolEgressCustomPatternsReplaceDefaults pins the override semantics:
// a non-empty pattern list REPLACES the built-in classification — the
// built-in web_search is no longer networked, the configured glob is.
func TestToolEgressCustomPatternsReplaceDefaults(t *testing.T) {
	gw := newToolEgressGateway(t, &config.ToolEgressConfig{
		Action:            "strip",
		NetworkedPatterns: []string{"internal_http*"},
	})
	p := &Proxy{}
	payload := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"tools": []any{
			toolDecl("web_search"),
			toolDecl("internal_http_post"),
			toolDecl("local_lookup"),
		},
	}
	if _, ok := p.applyToolEgressPolicy(gw, httptest.NewRecorder(), payload, "demo-agent", true); !ok {
		t.Fatal("strip must continue the dispatch")
	}
	tools := payload["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, declaredToolName(tl.(map[string]any)))
	}
	if len(names) != 2 || names[0] != "web_search" || names[1] != "local_lookup" {
		t.Fatalf("custom patterns stripped the wrong set: %v", names)
	}
}

// TestToolEgressPruneInjectedMCPTools pins the execution-gate closure:
// a strip must also remove the networked names from the injected-MCP
// bookkeeping the loop's execution gate keys on (NENYA-162) — otherwise
// a model-initiated call to the stripped tool would still dispatch even
// though its declaration is gone.
func TestToolEgressPruneInjectedMCPTools(t *testing.T) {
	gw := newToolEgressGateway(t, &config.ToolEgressConfig{Action: "strip"})
	p := &Proxy{}
	payload := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"tools": []any{
			toolDecl("local_lookup"),
			toolDecl("web_search"),
		},
	}
	stripped, ok := p.applyToolEgressPolicy(gw, httptest.NewRecorder(), payload, "demo-agent", true)
	if !ok {
		t.Fatal("strip must continue the dispatch")
	}
	if len(stripped) != 1 || stripped[0] != "web_search" {
		t.Fatalf("stripped = %v, want [web_search]", stripped)
	}
	injected := []string{"web_search", "db_query", "web_search"}
	pruned := pruneInjectedMCPTools(injected, stripped)
	if len(pruned) != 1 || pruned[0] != "db_query" {
		t.Fatalf("pruned injected set = %v, want [db_query]", pruned)
	}
	// Non-stripped inputs pass through untouched.
	if got := pruneInjectedMCPTools([]string{"a", "b"}, nil); len(got) != 2 {
		t.Fatalf("prune without strippings mutated the set: %v", got)
	}
}

// TestToolEgressEndToEndStripOnRedactedChat pins the full wiring: a chat
// request whose content trips the secret-pattern redactor (sensitivity
// signal) and declares a networked tool has that tool stripped BEFORE
// dispatch — the upstream never sees it — while the local tool and the
// redacted message continue.
func TestToolEgressEndToEndStripOnRedactedChat(t *testing.T) {
	const secretKey = "sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ1234"
	var upstreamPayload struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []map[string]any `json:"messages"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &upstreamPayload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(6000)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(1000000)
	cfg.Bouncer.Enabled = config.PtrTo(true)
	cfg.Bouncer.RedactPatterns = []string{`sk-[a-zA-Z0-9]{20,}`}
	cfg.Bouncer.RedactionLabel = "REDACTED"
	cfg.Governance.ToolEgress = &config.ToolEgressConfig{Action: "strip"}
	cfg.Providers = map[string]config.ProviderConfig{
		"openai": {URL: upstream.URL + "/v1/chat/completions", AuthStyle: "bearer"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"demo-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "openai", Model: "demo-chat-model"}},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{"openai": "test-api-key"},
	}
	config.ModelRegistry["demo-chat-model"] = config.ModelEntry{Provider: "openai"}
	t.Cleanup(func() { delete(config.ModelRegistry, "demo-chat-model") })

	gw := gateway.New(t.Context(), *cfg, secrets, slog.Default())
	chain := pipeline.NewInterceptorChain(slog.Default())
	chain.Register(pipeline.NewRedactInterceptor(true, gw.SecretPatterns, cfg.Bouncer.RedactionLabel, nil))
	gw.InterceptorChain = chain

	p := &Proxy{}
	p.StoreGateway(gw)

	body := fmt.Sprintf(`{"model":"demo-agent","messages":[{"role":"user","content":"rotate %s now"}],"tools":[{"type":"function","function":{"name":"local_lookup","description":"d"}},{"type":"function","function":{"name":"web_search","description":"d"}}]}`, secretKey)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	names := make([]string, 0, len(upstreamPayload.Tools))
	for _, tl := range upstreamPayload.Tools {
		names = append(names, tl.Function.Name)
	}
	if len(names) != 1 || names[0] != "local_lookup" {
		t.Fatalf("upstream tools = %v, want only local_lookup (web_search must be stripped)", names)
	}
	for _, msg := range upstreamPayload.Messages {
		if content, _ := msg["content"].(string); strings.Contains(content, secretKey) {
			t.Fatalf("raw secret reached the upstream: %q", content)
		}
	}
}
