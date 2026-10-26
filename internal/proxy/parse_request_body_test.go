package proxy

import (
	"net/http/httptest"
	"testing"
)

// TestParseRequestBodyAttributesNativeShape pins AUD-24's lossy predicate
// (NENYA-257): once the hot-path peek identifies the native Anthropic shape
// and the conversion SUCCEEDS, the source format is "anthropic" — the client
// spoke Anthropic regardless of whether the converter's output happened to
// differ byte-for-byte. Pre-fix the attribution required converted bytes !=
// original bytes, so a native-shape body the converter returned unchanged
// (early exits: no messages) was mis-attributed "openai" — the client would
// have received OpenAI-shaped responses for an Anthropic-shaped conversation.
func TestParseRequestBodyAttributesNativeShape(t *testing.T) {
	p, _ := newTestProxy(t)

	// Native landmarks ("type") present, no messages: the converter returns
	// the body unchanged (early exit), but the client's wire shape is
	// Anthropic.
	body := []byte(`{"model":"m","type":"message","system":"s"}`)
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	w := httptest.NewRecorder()

	payload, sourceFormat, herr := p.parseRequestBody(p.Gateway(), w, r, body)
	if herr != nil {
		t.Fatalf("parseRequestBody: %v", herr)
	}
	if payload == nil {
		t.Fatal("payload must parse")
	}
	if sourceFormat != "anthropic" {
		t.Fatalf("sourceFormat = %q, want \"anthropic\" (the peek identified the native shape; byte-equality is not the test)", sourceFormat)
	}
}

// TestParseRequestBodyOpenAIStaysOpenAI guards the other side: an OpenAI
// body (no native landmarks) keeps "openai".
func TestParseRequestBodyOpenAIStaysOpenAI(t *testing.T) {
	p, _ := newTestProxy(t)

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	payload, sourceFormat, herr := p.parseRequestBody(p.Gateway(), w, r, body)
	if herr != nil {
		t.Fatalf("parseRequestBody: %v", herr)
	}
	if payload == nil {
		t.Fatal("payload must parse")
	}
	if sourceFormat != "openai" {
		t.Fatalf("sourceFormat = %q, want \"openai\"", sourceFormat)
	}
}
