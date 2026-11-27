package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"unicode/utf8"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
)

// NENYA-352 (Module A Phase 050): the non-chat endpoints — embeddings,
// files, batches, audio — carried user content to providers with NO content
// policy: the interceptor chain (secret redaction, entropy redaction,
// injection detection) ran on the chat routes only. This file extends the
// deterministic security subset (gw.MCPSecurityChain — the same fail-closed
// interceptors the MCP loop's re-scan uses) to those endpoints' text
// surfaces:
//
//   - /v1/embeddings: the input string (or string array) — wrapped as
//     messages, scanned, written back into the payload, re-marshaled.
//   - /v1/audio/speech + the other JSON extension endpoints: the text
//     fields the OpenAI shapes define (input, prompt, query, documents).
//   - /v1/audio/transcriptions: multipart form fields (prompt) and the
//     uploaded audio is binary — no text surface (documented skip).
//   - /v1/files uploads: multipart form fields + text-decodable file parts
//     (batch JSONL, plain text); binary parts pass through byte-exact.
//   - /v1/batches create: references an uploaded file id — its content was
//     scanned at upload; the create body itself carries no model content.
//
// Semantics mirror the chat path and the MCP re-scan: strict interceptor
// faults render 503 via writePipelineRejection, deterministic injection
// rejections render 403; redaction/entropy rewrite the surfaces in place
// and the request continues with the rewritten body. Escalation is
// disabled (DisableEscalation) — the tier-1 deterministic verdict applies;
// these surfaces never open LLM classifier hops. A nil chain fails open
// (gateway built without an interceptor chain — the pre-NENYA-136
// convention shared with the MCP re-scan).

// policyTextFields are the JSON text surfaces scanned on the generic
// extension endpoints. Array-of-strings values (documents, multi-input)
// are scanned per element.
var policyTextFields = []string{"input", "prompt", "query", "documents"}

// policySurface locates one scanned text value inside a payload: the
// field, and the array index (-1 for scalar values).
type policySurface struct {
	field string
	index int
}

// applyMessagesContentPolicy runs the deterministic security subset over
// one text surface expressed as message maps (the interceptors' native
// shape). Returns the (possibly rewritten) messages; ok=false means the
// response was already written (rejection or strict fault).
func (p *Proxy) applyMessagesContentPolicy(gw *gateway.NenyaGateway, ctx context.Context, w http.ResponseWriter, agentName string, messages []map[string]any) ([]map[string]any, bool) {
	chain := gw.MCPSecurityChain
	if chain == nil {
		return messages, true
	}
	req := &pipeline.InterceptRequest{
		Payload:           map[string]any{"model": agentName},
		Messages:          messages,
		AgentName:         agentName,
		DisableEscalation: true,
	}
	if _, err := chain.Execute(ctx, req); err != nil {
		writePipelineRejection(gw, w, err)
		return nil, false
	}
	return messages, true
}

// collectPolicySurfaces lists the text surfaces present in a payload (the
// policyTextFields scalars and string-array elements).
func collectPolicySurfaces(payload map[string]any) []policySurface {
	var surfaces []policySurface
	for _, field := range policyTextFields {
		switch v := payload[field].(type) {
		case string:
			if v != "" {
				surfaces = append(surfaces, policySurface{field: field, index: -1})
			}
		case []interface{}:
			for i, item := range v {
				if s, isStr := item.(string); isStr && s != "" {
					surfaces = append(surfaces, policySurface{field: field, index: i})
				}
			}
		}
	}
	return surfaces
}

// scanPayloadTextFields scans the policyTextFields surfaces of a parsed
// JSON payload in place and reports whether the payload changed. The
// caller re-marshals when changed. ok=false means the response was
// already written.
func (p *Proxy) scanPayloadTextFields(gw *gateway.NenyaGateway, ctx context.Context, w http.ResponseWriter, agentName string, payload map[string]any) (changed, ok bool) {
	surfaces := collectPolicySurfaces(payload)
	if len(surfaces) == 0 {
		return false, true
	}
	messages := make([]map[string]any, 0, len(surfaces))
	for _, s := range surfaces {
		messages = append(messages, map[string]any{"role": "user", "content": surfaceValue(payload, s)})
	}
	cleaned, ok := p.applyMessagesContentPolicy(gw, ctx, w, agentName, messages)
	if !ok {
		return false, false
	}
	for i, msg := range cleaned {
		if writeBackSurface(payload, surfaces[i], msg) {
			changed = true
		}
	}
	return changed, true
}

// surfaceValue reads a surface's text from the payload.
func surfaceValue(payload map[string]any, s policySurface) string {
	if s.index < 0 {
		v, _ := payload[s.field].(string)
		return v
	}
	arr, _ := payload[s.field].([]interface{})
	if s.index < len(arr) {
		v, _ := arr[s.index].(string)
		return v
	}
	return ""
}

// writeBackSurface writes one scanned message's content back into its
// payload surface, preserving shape, and reports whether it changed.
func writeBackSurface(payload map[string]any, s policySurface, msg map[string]any) bool {
	content, isStr := msg["content"].(string)
	if !isStr {
		return false
	}
	if s.index < 0 {
		if payload[s.field] != content {
			payload[s.field] = content
			return true
		}
		return false
	}
	arr, isArray := payload[s.field].([]interface{})
	if isArray && s.index < len(arr) && arr[s.index] != content {
		payload[s.field].([]interface{})[s.index] = content
		return true
	}
	return false
}

// storedPart is one parsed multipart part held for re-emission.
type storedPart struct {
	formName    string
	fileName    string
	contentType string // the part's original Content-Type ("" when absent)
	data        []byte
}

// scanMultipartBody scans a multipart body's text surfaces: every form
// field, and file parts whose content is text-like (batch JSONL, plain
// text). Binary parts pass through byte-exact (documented skip). The body
// is re-emitted with a fresh boundary when anything changed; ok=false
// means the response was already written. Bodies that do not parse as the
// declared multipart pass through verbatim (the upstream rejects
// malformed bodies; the policy never corrupts).
func (p *Proxy) scanMultipartBody(gw *gateway.NenyaGateway, ctx context.Context, w http.ResponseWriter, contentType string, body []byte, agentName string) ([]byte, string, bool) {
	parts, ok := parseMultipartParts(contentType, body, gw)
	if !ok {
		return body, contentType, true
	}
	changed, ok := p.scanMultipartParts(gw, ctx, w, agentName, parts)
	if !ok {
		return nil, "", false
	}
	if !changed {
		return body, contentType, true
	}
	return reemitMultipartParts(parts, body, contentType)
}

// parseMultipartParts parses a multipart body into held parts; ok=false
// means the body is not multipart or does not parse (passthrough).
func parseMultipartParts(contentType string, body []byte, gw *gateway.NenyaGateway) ([]storedPart, bool) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		return nil, false
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var parts []storedPart
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return parts, true
		}
		if err != nil {
			// Malformed multipart — passthrough verbatim (the upstream
			// rejects malformed bodies; the policy never corrupts).
			gw.Logger.Debug("content policy skipped a malformed multipart body", "err", err)
			return nil, false
		}
		data, readErr := io.ReadAll(part)
		if readErr != nil {
			return nil, false
		}
		parts = append(parts, storedPart{
			formName:    part.FormName(),
			fileName:    part.FileName(),
			contentType: part.Header.Get("Content-Type"),
			data:        data,
		})
	}
}

// scanMultipartParts runs the policy over the text surfaces of the held
// parts: form fields and file parts alike must be text-like (binary form
// fields pass through unscanned exactly like binary file parts — running
// the entropy redactor over binary field bytes would corrupt them), and
// empty parts skip. Mutating data in place. changed reports whether any
// part's bytes changed; ok=false means the response was already written.
func (p *Proxy) scanMultipartParts(gw *gateway.NenyaGateway, ctx context.Context, w http.ResponseWriter, agentName string, parts []storedPart) (changed, ok bool) {
	for i := range parts {
		part := &parts[i]
		if !isTextLike(part.data) {
			continue
		}
		if len(part.data) == 0 {
			continue
		}
		messages := []map[string]any{{"role": "user", "content": string(part.data)}}
		cleaned, scanOK := p.applyMessagesContentPolicy(gw, ctx, w, agentName, messages)
		if !scanOK {
			return false, false
		}
		cleanedContent, isStr := cleaned[0]["content"].(string)
		if !isStr || cleanedContent == string(part.data) {
			continue
		}
		part.data = []byte(cleanedContent)
		changed = true
	}
	return changed, true
}

// reemitMultipartParts re-serializes the parts with a fresh boundary.
// File parts re-emit with their ORIGINAL Content-Type (via CreatePart with
// a reconstructed header — CreateFormFile would hardcode octet-stream and
// drift from providers that validate part-MIME↔extension consistency),
// falling back to octet-stream when the upload carried none. On any
// emission failure the ORIGINAL body/content-type return unchanged.
func reemitMultipartParts(parts []storedPart, origBody []byte, origCT string) ([]byte, string, bool) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, part := range parts {
		h := textproto.MIMEHeader{}
		if part.fileName != "" {
			disposition := fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
				escapeMultipartQuotes(part.formName), escapeMultipartQuotes(part.fileName))
			h.Set("Content-Disposition", disposition)
			ct := part.contentType
			if ct == "" {
				ct = "application/octet-stream"
			}
			h.Set("Content-Type", ct)
		} else {
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"`, escapeMultipartQuotes(part.formName)))
		}
		fw, err := mw.CreatePart(h)
		if err != nil {
			return origBody, origCT, true
		}
		if _, err := fw.Write(part.data); err != nil {
			return origBody, origCT, true
		}
	}
	if err := mw.Close(); err != nil {
		return origBody, origCT, true
	}
	return buf.Bytes(), mw.FormDataContentType(), true
}

// escapeMultipartQuotes escapes the quoted-string specials for a
// Content-Disposition value (RFC 7578 §4.4: quote and backslash).
func escapeMultipartQuotes(v string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return replacer.Replace(v)
}

// isTextLike reports whether part content is decodable text for policy
// scanning: valid UTF-8 with no NUL byte anywhere (binary formats —
// audio, PDFs, images — fail one of the two and pass through untouched).
func isTextLike(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	return utf8.Valid(data)
}

// remarshalPolicyPayload re-marshals a scanned payload map for dispatch.
func remarshalPolicyPayload(payload map[string]any) ([]byte, error) {
	return json.Marshal(payload)
}

// embeddingsInputMessages wraps the embeddings input surface as message
// maps for the policy chain: string input becomes one message, a string
// array becomes one message per element (order preserved), and tokenized
// int-array inputs have no text surface (false — documented skip; they are
// never scanned).
func embeddingsInputMessages(input any) ([]map[string]any, bool) {
	switch v := input.(type) {
	case string:
		if v == "" {
			return nil, false
		}
		return []map[string]any{{"role": "user", "content": v}}, true
	case []interface{}:
		var messages []map[string]any
		text := false
		for _, item := range v {
			if s, ok := item.(string); ok {
				messages = append(messages, map[string]any{"role": "user", "content": s})
				text = true
			}
		}
		return messages, text
	default:
		return nil, false
	}
}

// writeBackEmbeddingsInput writes the scanned message contents back into
// payload["input"], preserving the wire shape (string vs array; non-string
// array elements untouched) and reporting whether anything changed.
func writeBackEmbeddingsInput(payload map[string]any, messages []map[string]any) bool {
	changed := false
	switch input := payload["input"].(type) {
	case string:
		if len(messages) > 0 {
			if content, ok := messages[0]["content"].(string); ok && content != input {
				payload["input"] = content
				changed = true
			}
		}
	case []interface{}:
		mi := 0
		for i, item := range input {
			if _, isStr := item.(string); !isStr {
				continue
			}
			if mi >= len(messages) {
				break
			}
			if content, ok := messages[mi]["content"].(string); ok && content != item {
				input[i] = content
				changed = true
			}
			mi++
		}
	}
	return changed
}

// scanEmbeddingsInput runs the content policy over the embeddings input
// surface and returns the body to dispatch (re-marshaled from the scanned
// payload when anything changed). ok=false means the response was already
// written. A scanned payload that fails to re-encode fails CLOSED: a
// body the policy rewrote must never dispatch in the clear.
func (p *Proxy) scanEmbeddingsInput(gw *gateway.NenyaGateway, ctx context.Context, w http.ResponseWriter, payload map[string]any, bodyBytes []byte) ([]byte, bool) {
	inputMsgs, wrapOK := embeddingsInputMessages(payload["input"])
	if !wrapOK {
		return bodyBytes, true
	}
	cleaned, ok := p.applyMessagesContentPolicy(gw, ctx, w, "", inputMsgs)
	if !ok {
		return nil, false
	}
	if !writeBackEmbeddingsInput(payload, cleaned) {
		return bodyBytes, true
	}
	newBody, err := remarshalPolicyPayload(payload)
	if err != nil {
		gw.Logger.Error("scanned embeddings payload failed to re-marshal; refusing the dispatch", "err", err)
		writeStructuredError(w, http.StatusInternalServerError, infra.ErrorKindInternal, "request could not be re-encoded after scanning")
		return nil, false
	}
	return newBody, true
}

// scanExtensionBody runs the content policy over an extension endpoint's
// request body and returns the body + content type to dispatch: JSON
// bodies scan their policyTextFields (speech input, moderations input,
// rerank query/documents); multipart bodies (transcriptions) scan form
// fields — the audio itself is binary and passes through byte-exact.
// Non-JSON, non-multipart bodies and bodies that do not parse as JSON
// objects pass through verbatim. ok=false means the response was already
// written.
func (p *Proxy) scanExtensionBody(gw *gateway.NenyaGateway, ctx context.Context, w http.ResponseWriter, contentType string, bodyBytes []byte) ([]byte, string, bool) {
	if strings.HasPrefix(strings.ToLower(contentType), "multipart/") {
		return p.scanMultipartBody(gw, ctx, w, contentType, bodyBytes, "")
	}
	if !strings.Contains(strings.ToLower(contentType), "json") {
		return bodyBytes, contentType, true
	}
	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil || payload == nil {
		return bodyBytes, contentType, true
	}
	changed, ok := p.scanPayloadTextFields(gw, ctx, w, "", payload)
	if !ok {
		return nil, "", false
	}
	if !changed {
		return bodyBytes, contentType, true
	}
	newBody, err := remarshalPolicyPayload(payload)
	if err != nil {
		gw.Logger.Error("scanned extension payload failed to re-marshal; refusing the dispatch", "err", err)
		writeStructuredError(w, http.StatusInternalServerError, infra.ErrorKindInternal, "request could not be re-encoded after scanning")
		return nil, "", false
	}
	return newBody, contentType, true
}
