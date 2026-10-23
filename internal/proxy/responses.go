package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/util"
)

// responsesModelRequired reports whether the request targets the create
// endpoint (POST /v1/responses), which must name a model so per-key agent
// scoping can be evaluated. The canonical path is used (path.Clean), so
// non-canonical spellings of the create endpoint cannot slip past as
// id-addressed operations. Sub-paths are id-addressed operations (retrieve,
// cancel, delete, compact) that carry no model; the upstream enforces
// ownership of the id (NENYA-189).
func responsesModelRequired(r *http.Request) bool {
	clean, ok := canonicalResponsesPath(r.URL.Path)
	return ok && r.Method == http.MethodPost && clean == "/v1/responses"
}

// responsesModelError is the shared fail-closed rejection for a request whose
// model cannot be read, keeping the /v1/responses message identical to the
// chat route's.
func responsesModelError() *httpError {
	return &httpError{Code: http.StatusBadRequest, Message: `Missing or empty "model" field in request body`, Kind: infra.ErrorKindInvalidRequest}
}

// authorizeResponsesAgent enforces per-key agent scoping on /v1/responses.
// The create endpoint must carry a model that can be read as a non-empty
// string; a body that is not a JSON object, a model of the wrong type, or a
// missing model on the create endpoint is rejected instead of forwarded
// unscoped (NENYA-196, fail closed). Id-addressed operations pass through.
//
// Note on scoping semantics: allowed_agents holds agent names and is matched
// against the request's model, so a key scoped to agent names is denied a
// plain upstream model id, while a key with an empty list reaches whatever
// provider the model (or the default-provider fallback) resolves to.
func (p *Proxy) authorizeResponsesAgent(gw *gateway.NenyaGateway, r *http.Request, apiKey *config.ApiKey, bodyBytes []byte) (string, *httpError) {
	// The nil-key check comes first: every unauthenticated call is answered on
	// the same observable path (403 / invalid_key / fixed label) instead of
	// falling into the model-shaped denials, which need a real key to record.
	if apiKey == nil {
		gw.Metrics.IncAuthDenials("unauthenticated", "invalid_key")
		p.logAuthWarning(gw, "unscoped /v1/responses request denied (invalid_key)", r)
		return "", &httpError{Code: http.StatusForbidden, Message: "Forbidden", Kind: infra.ErrorKindAuthFailed}
	}

	// A path that does not canonicalize under /v1/responses (traversal,
	// malformed escapes) is refused here as well as by the handler's safety
	// check: scoping cannot be evaluated for it, and passing it through would
	// make the authorized shape depend on which layer ran first.
	if _, ok := canonicalResponsesPath(r.URL.Path); !ok {
		return "", p.rejectResponsesScoping(gw, r, apiKey, "invalid_body",
			&httpError{Code: http.StatusBadRequest, Message: "Invalid request path", Kind: infra.ErrorKindInvalidRequest})
	}

	body := bytes.TrimSpace(bodyBytes)
	if len(body) == 0 {
		if responsesModelRequired(r) {
			return "", p.rejectResponsesScoping(gw, r, apiKey, "missing_model", responsesModelError())
		}
		return "", nil
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", p.rejectResponsesScoping(gw, r, apiKey, "invalid_body",
			&httpError{Code: http.StatusBadRequest, Message: "Request body must be a JSON object", Kind: infra.ErrorKindInvalidRequest})
	}
	rawModel, hasModel := payload["model"]
	if !hasModel {
		if responsesModelRequired(r) {
			return "", p.rejectResponsesScoping(gw, r, apiKey, "missing_model", responsesModelError())
		}
		return "", nil
	}
	modelName, ok := rawModel.(string)
	if !ok || modelName == "" {
		return "", p.rejectResponsesScoping(gw, r, apiKey, "missing_model", responsesModelError())
	}

	if !auth.AuthorizeAgent(apiKey, modelName) {
		gw.Metrics.IncAuthDenials(apiKey.Name, "agent")
		p.logAuthDenial(gw, apiKey, "agent "+modelName, r)
		return "", &httpError{Code: http.StatusForbidden, Message: "Forbidden", Kind: infra.ErrorKindAuthFailed}
	}
	return modelName, nil
}

// rejectResponsesScoping records and renders a fail-closed /v1/responses
// scoping denial (metric + log + structured 400), mirroring the chat route's
// missing-model path so the denial is observable.
func (p *Proxy) rejectResponsesScoping(gw *gateway.NenyaGateway, r *http.Request, apiKey *config.ApiKey, reason string, rendered *httpError) *httpError {
	gw.Metrics.IncAuthDenials(apiKeyName(apiKey), reason)
	p.logAuthDenial(gw, apiKey, reason+" on /v1/responses", r)
	return rendered
}

// handleResponses proxies the /v1/responses endpoint. NENYA-189 inventory
// decision: this is pure passthrough — the upstream provider issues and
// enforces ownership of every response id (previous_response_id, object ids),
// and Nenya keeps no state addressable by those ids. Response ids surfaced to
// clients are therefore NOT obfuscated: they never authorize anything on the
// Nenya side, so obfuscation would only break client-side correlation. The
// ids that DO index Nenya state (tool-call ids in the thought-signature
// cache, chat fingerprints in the response cache) are scope-partitioned by
// API-key identity — see thoughtSigScope and ResponseCache semantic
// namespaces. Known exception: the adapter registry's global Gemini adapter
// (adapter.InitWithDeps installs the live cache into a process-global
// singleton whose MutateRequest/MutateResponse Load/Store raw tool-call ids).
// No production caller invokes those methods (the MutateResponse shim wires a
// nil-cache path pre-init and the singleton post-init is only reached through
// tests); treat that surface as deprecated — do not add callers, and prefer
// the per-request scoped views above.
// NENYA-189 inventory note (known limitation): the MCP buffered loop replays
// raw upstream SSE and does not run this provider transformer, so Gemini
// thought signatures issued inside an MCP tool loop are not cached (they
// degrade to the NENYA-50 placeholder policy on the next turn) — a behavior
// gap to close separately, not an IDOR (nothing cross-key is readable).
func (p *Proxy) handleResponses(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
	keyRef := ""
	if apiKey != nil {
		keyRef = apiKey.Name
	}
	pathSafe := p.isPathSafeResponses(r.URL.Path)
	if !pathSafe {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Invalid path")
		return
	}

	bodyBytes, herr := p.readResponsesBody(gw, w, r)
	if renderBodyReadError(w, herr) {
		return
	}

	responsesModel, herr := p.authorizeResponsesAgent(gw, r, apiKey, bodyBytes)
	if herr != nil {
		writeHTTPError(w, herr)
		return
	}

	provider := p.resolveResponsesProviderFromModel(gw, responsesModel)
	if provider == nil {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindModelNotFound, util.ErrNoProvider)
		return
	}

	targetURL := p.resolveResponsesURL(provider, r.URL.Path, r.URL.RawQuery)
	if targetURL == "" {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Provider does not support responses API")
		return
	}

	ctx, cancel := p.buildResponsesContext(r, provider)
	defer cancel()

	if !gw.RateLimiter.Check(provider.Name, provider.BaseURL, 0) {
		writeStructuredError(w, http.StatusTooManyRequests, infra.ErrorKindRateLimited, "Rate limit exceeded")
		return
	}

	maxAttempts := provider.MaxRetryAttempts
	if maxAttempts <= 0 {
		maxAttempts = gw.Config.Governance.EffectiveMaxRetryAttempts()
	}

	gw.Stats.RecordRequest(responsesModel, 0)
	gw.Metrics.RecordUpstreamRequest(responsesModel, "", provider.Name)

	ctxLogger := gw.Logger.With("operation", "responses", "provider", provider.Name, "api_key", keyRef)

	resp, err := util.DoWithRetryResp(ctx, maxAttempts, func() (*http.Response, error) {
		req, reqErr := p.buildUpstreamRequest(gw, ctx, r.Method, targetURL, bodyBytes, provider.Name, responsesModel, "", r.Header)
		if reqErr != nil {
			return nil, reqErr
		}
		if len(bodyBytes) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}

		httpResp, fetchErr := gw.ClientFor(provider.Name).Do(req)
		if fetchErr != nil {
			if httpResp != nil {
				_ = httpResp.Body.Close()
			}
			return nil, fetchErr
		}
		if httpResp.StatusCode >= 500 {
			_ = httpResp.Body.Close()
			return nil, fmt.Errorf("upstream error: %d", httpResp.StatusCode)
		}
		return httpResp, nil
	})

	if err != nil {
		ctxLogger.Error("responses upstream request failed", "err", err)
		writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindNetworkError, "Upstream provider error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	writeUpstreamResponse(ctx, w, resp, ctxLogger)
}

// canonicalResponsesPath decodes and cleans a request path for the
// /v1/responses route. One canonicalizer serves the traversal check, the
// route-shape decision, and the upstream URL: a path that canonicalizes
// differently in any of them could be authorized as one shape and dispatched
// as another (NENYA-196, e.g. a dot segment hidden behind percent-encoding).
//
// Decoding iterates to a fixed point (bounded) before cleaning, so a
// multiply-encoded dot segment cannot survive as literal "%2e" text past the
// traversal guard or the route-shape decision.
func canonicalResponsesPath(pathStr string) (string, bool) {
	decoded := pathStr
	for i := 0; i < maxResponsesPathDecodePasses; i++ {
		next, err := url.PathUnescape(decoded)
		if err != nil {
			return "", false
		}
		if next == decoded {
			break
		}
		decoded = next
	}
	clean := path.Clean(decoded)
	if strings.Contains(clean, "..") || strings.Contains(decoded, "..") {
		return "", false
	}
	if !hasPathPrefix(clean, "/v1/responses") {
		return "", false
	}
	return clean, true
}

// maxResponsesPathDecodePasses bounds the fixed-point decode: legitimate
// clients never percent-encode twice, and the bound keeps a hostile path from
// forcing unbounded decoding work.
const maxResponsesPathDecodePasses = 4

// isPathSafeResponses checks if the decoded and cleaned path is safe (no path
// traversal) and has the expected /v1/responses prefix.
func (p *Proxy) isPathSafeResponses(pathStr string) bool {
	_, ok := canonicalResponsesPath(pathStr)
	return ok
}

func (p *Proxy) resolveResponsesProviderFromModel(gw *gateway.NenyaGateway, modelName string) *config.Provider {
	if modelName != "" {
		matches := routing.ResolveProviders(modelName, gw.Providers, gw.ModelCatalog)
		if len(matches) > 0 {
			if p, ok := gw.Providers[matches[0].Provider]; ok {
				return p
			}
		}
	}

	return p.getDefaultResponseProvider(gw)
}

func (p *Proxy) readResponsesBody(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) ([]byte, *httpError) {
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		return []byte{}, nil
	}

	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	return readRequestBody(r, gw, "failed to read responses request body")
}

func (p *Proxy) resolveResponsesURL(provider *config.Provider, pathStr, query string) string {
	// Fallback: BaseURL → trimmed URL → empty (provider must have BaseURL or URL ending with /v1)
	baseURL := strings.TrimSuffix(provider.BaseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimSuffix(provider.URL, "/chat/completions")
	}
	if baseURL == provider.URL || baseURL == "" {
		return ""
	}

	// Canonicalize the sub-path with the same helper the route-shape decision
	// uses, so the endpoint dispatched upstream can never diverge from the
	// route shape that was authorized (NENYA-196). A path that does not
	// canonicalize under /v1/responses is refused rather than rewritten into a
	// nonsense upstream URL. Note: canonicalizing normalizes trailing slashes
	// on sub-paths (e.g. /resp_1/ -> /resp_1).
	// Unreachable in practice: handleResponses rejects a non-canonicalizable
	// path via isPathSafeResponses first, so this guard only protects a future
	// caller from building a nonsense upstream URL.
	cleanPath, ok := canonicalResponsesPath(pathStr)
	if !ok {
		return ""
	}
	subPath := strings.TrimPrefix(cleanPath, "/v1/responses")
	var target string
	if strings.HasSuffix(baseURL, "/v1") {
		target = baseURL + "/responses" + subPath
	} else {
		target = baseURL + "/v1/responses" + subPath
	}
	if query != "" {
		target += "?" + query
	}
	return target
}

func (p *Proxy) buildResponsesContext(r *http.Request, provider *config.Provider) (context.Context, func()) {
	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(provider.TimeoutSeconds)*time.Second)
		return timeoutCtx, cancel
	}
	return ctx, func() {}
}

func (p *Proxy) getDefaultResponseProvider(gw *gateway.NenyaGateway) *config.Provider {
	preferred := []string{"deepseek", "openai", "anthropic", "openrouter"}
	for _, name := range preferred {
		if pr, ok := gw.Providers[name]; ok && pr.APIKey != "" {
			return pr
		}
	}

	// Deterministic fallback: gw.Providers is a map, so iterate a sorted key
	// list rather than the map (the chosen provider must not vary per request).
	names := make([]string, 0, len(gw.Providers))
	for name := range gw.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pr := gw.Providers[name]
		if pr.APIKey != "" && pr.AuthStyle != config.AuthStyleNone {
			return pr
		}
	}

	for _, name := range names {
		if gw.Providers[name].AuthStyle == config.AuthStyleNone {
			return gw.Providers[name]
		}
	}
	return nil
}
