package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/jsonschema"
)

// deepSchema renders a schema nesting deeper than the budget allows.
func deepSchema(depth int) string {
	schema := `{"type":"string"}`
	for i := 0; i < depth; i++ {
		schema = `{"type":"object","properties":{"n":` + schema + `}}`
	}
	return schema
}

func healthyGW(t *testing.T) *gateway.NenyaGateway {
	t.Helper()
	return &gateway.NenyaGateway{
		Logger: testLog(t),
		Config: config.Config{Server: config.ServerConfig{MaxBodyBytes: 1 << 20}},
	}
}

func TestCheckSchemaBudget(t *testing.T) {
	gw := healthyGW(t)

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "no tools",
			body: `{"model":"m","messages":[]}`,
		},
		{
			name: "realistic openai tool",
			body: `{"model":"m","tools":[{"type":"function","function":{"name":"read_file",` +
				`"parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`,
		},
		{
			name: "anthropic-shaped tool in a native body",
			body: `{"model":"m","system":"s","tools":[{"name":"read_file","input_schema":{"type":"object"}}]}`,
		},
		{
			name: "tools that are not a list pass to the decoder",
			body: `{"model":"m","tools":{"function":{}}}`,
		},
		{
			name: "response_format without a json schema",
			body: `{"model":"m","response_format":{"type":"json_object"}}`,
		},
		{
			name:    "deep openai tool schema",
			wantErr: true,
			body: `{"model":"m","tools":[{"type":"function","function":{"name":"deep","parameters":` +
				deepSchema(jsonschema.MaxInputDepth+1) + `}}]}`,
		},
		{
			name:    "deep native anthropic tool schema",
			wantErr: true,
			// The native body is converted and sanitized before the payload
			// exists, so this only fails if the guard reads the raw body.
			body: `{"model":"m","system":"s","tools":[{"name":"deep","input_schema":` +
				deepSchema(jsonschema.MaxInputDepth+1) + `}]}`,
		},
		{
			name:    "deep structured-output schema",
			wantErr: true,
			body: `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"out","schema":` +
				deepSchema(jsonschema.MaxInputDepth+1) + `}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			herr := checkSchemaBudget(gw, []byte(tt.body))
			if !tt.wantErr {
				if herr != nil {
					t.Fatalf("expected no rejection, got %+v", herr)
				}
				return
			}
			if herr == nil {
				t.Fatal("expected a rejection")
			}
			if herr.Code != http.StatusBadRequest || herr.Kind != infra.ErrorKindInvalidRequest {
				t.Fatalf("got %d/%s, want 400/%s", herr.Code, herr.Kind, infra.ErrorKindInvalidRequest)
			}
			if !strings.Contains(herr.Message, "depth") {
				t.Fatalf("message must name the exceeded dimension: %q", herr.Message)
			}
		})
	}
}

// TestValidateChatRequest_RejectsOversizedSchema pins NENYA-199 end to end: an
// adversarial schema is refused with a structured 400 before routing —
// including on a native Anthropic body, whose schemas the conversion+
// sanitization would otherwise shrink below the cap.
func TestValidateChatRequest_RejectsOversizedSchema(t *testing.T) {
	gw := healthyGW(t)
	p := &Proxy{}

	tests := []struct {
		name string
		body string
	}{
		{
			name: "openai tool",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function",` +
				`"function":{"name":"deep","parameters":` + deepSchema(jsonschema.MaxInputDepth+5) + `}}]}`,
		},
		{
			name: "native anthropic tool",
			body: `{"model":"m","system":"s","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"name":"deep","input_schema":` + deepSchema(jsonschema.MaxInputDepth+5) + `}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tt.body))
			w := httptest.NewRecorder()

			_, herr := p.validateChatRequest(w, r, gw, nil)
			if herr == nil {
				t.Fatal("expected the oversized schema to be rejected")
			}
			if herr.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (%+v)", herr.Code, herr)
			}
			if herr.Kind != infra.ErrorKindInvalidRequest {
				t.Fatalf("error_kind = %q, want %q", herr.Kind, infra.ErrorKindInvalidRequest)
			}
			if !strings.Contains(herr.Message, `"deep"`) {
				t.Fatalf("message must name the offending tool: %q", herr.Message)
			}
		})
	}
}
