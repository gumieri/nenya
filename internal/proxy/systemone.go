package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/util"
)

// systemOneURLCacheKey is the provider-level format key holding the System One
// decision endpoint (TypeSafe Jev). Providers that serve System One models
// declare it under FormatURLs; when absent, the client-supplied model name is
// resolved through the catalog to a provider that does.
const systemOneURLCacheKey = config.FormatKeySystemOne

// handleSystemOne proxies System One decision requests (TypeSafe Jev) to the
// provider's decision endpoint. The body is forwarded verbatim (it is not an
// OpenAI chat body: it carries `state` and typed `questions`), the provider
// key is injected, and the non-streaming JSON response is relayed. Nenya
// client authentication and RBAC are enforced by the route chain; usage is
// recorded from the response's `usage` object.
func (p *Proxy) handleSystemOne(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
	keyRef := ""
	if apiKey != nil {
		keyRef = apiKey.Name
	}

	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	bodyBytes, herr := readRequestBody(r, gw, "failed to read system one request body")
	if renderBodyReadError(w, herr) {
		return
	}

	modelName, herr := parseSystemOneModel(gw, bodyBytes)
	if herr != nil {
		writeStructuredError(w, herr.Code, herr.Kind, herr.Message)
		return
	}

	provider, herr := resolveSystemOneProvider(gw, modelName)
	if herr != nil {
		writeStructuredError(w, herr.Code, herr.Kind, herr.Message)
		return
	}

	endpoint := provider.FormatURLs[systemOneURLCacheKey]
	if endpoint == "" {
		gw.Logger.Warn("provider has no system one endpoint configured", "provider", provider.Name, "model", modelName)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindModelNotFound, "Provider does not support System One decision requests")
		return
	}

	tokenCount := gw.CountTokens(string(bodyBytes))

	// NENYA-336: pass the REAL token count — the body is fully buffered, so
	// the count is computed from it directly (still a cl100k estimate that
	// drifts against the provider, corrected by the response true-up in the
	// forward helper) and the TPM dimension is enforced here like on the
	// chat path (a hardcoded 0 skipped TPM entirely, letting System One
	// traffic run RPM-only).
	if allowed, rej := gw.RateLimiter.CheckDetailed(provider.Name, provider.BaseURL, tokenCount); !allowed {
		writeRateLimitRejection(gw.Logger, w, provider.Name, rej)
		return
	}

	gw.Stats.RecordRequest(modelName, tokenCount)
	gw.Metrics.RecordDecision(modelName, provider.Name)
	gw.Metrics.RecordTokens("client", modelName, "", provider.Name, tokenCount)

	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), time.Duration(provider.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	maxAttempts := provider.MaxRetryAttempts
	if maxAttempts <= 0 {
		maxAttempts = gw.Config.Governance.EffectiveMaxRetryAttempts()
	}

	p.forwardSystemOneRequest(forwardOpts{
		gw: gw, respWriter: w, requestCtx: ctx, endpointURL: endpoint, bodyBytes: bodyBytes,
		providerName: provider.Name, baseURL: provider.BaseURL, modelName: modelName, srcHeaders: r.Header,
		maxAttempts: maxAttempts, keyRef: keyRef, estimate: tokenCount,
	})
}

// parseSystemOneModel extracts and validates the "model" field from a System
// One request body.
func parseSystemOneModel(gw *gateway.NenyaGateway, bodyBytes []byte) (string, *httpError) {
	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		gw.Logger.Warn("failed to parse system one JSON", "err", err)
		return "", &httpError{Code: http.StatusBadRequest, Kind: infra.ErrorKindInvalidRequest, Message: "Invalid JSON payload"}
	}
	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		gw.Logger.Warn("missing or empty model in system one request")
		return "", &httpError{Code: http.StatusBadRequest, Kind: infra.ErrorKindInvalidRequest, Message: `Missing or empty "model" field`}
	}
	if len(modelName) > MaxModelNameLength {
		gw.Logger.Warn("model name exceeds maximum length in system one request", "length", len(modelName))
		return "", &httpError{Code: http.StatusBadRequest, Kind: infra.ErrorKindInvalidRequest, Message: "Model name too long"}
	}
	return modelName, nil
}

// resolveSystemOneProvider resolves the provider for a System One model. A
// non-chat model (which is what System One models are) is resolved even though
// it is absent from the merged chat catalog: the request's model field selects
// the provider directly when exactly one configured provider declares the
// endpoint, and otherwise falls back to catalog resolution.
func resolveSystemOneProvider(gw *gateway.NenyaGateway, modelName string) (*config.Provider, *httpError) {
	if matches := routing.ResolveProviders(modelName, gw.Providers, gw.ModelCatalog); len(matches) > 0 {
		if p, ok := gw.Providers[matches[0].Provider]; ok {
			return p, nil
		}
	}

	// System One models are excluded from the chat catalog. Resolve by the
	// provider that actually serves them as non-chat models.
	owner, owners := systemOneProviderFor(gw, modelName)
	switch len(owners) {
	case 0:
		gw.Logger.Warn("no provider for system one model", "model", modelName)
		return nil, &httpError{Code: http.StatusBadRequest, Kind: infra.ErrorKindModelNotFound, Message: util.ErrNoProvider}
	case 1:
		return owners[0], nil
	default:
		// Ambiguous: more than one provider classifies the model and declares a
		// System One endpoint (e.g. two Jev resellers). Select the
		// lexicographically first so the owner is deterministic across restarts
		// and reloads (a Go map iteration order would otherwise send the same
		// decision payload to a different vendor each time).
		gw.Logger.Warn("multiple providers serve system one model; using the first by name", "model", modelName, "provider", owner)
		return owners[0], nil
	}
}

// systemOneProviderFor returns the name and Providers that classify the model
// as non-chat and declare a System One endpoint.
func systemOneProviderFor(gw *gateway.NenyaGateway, modelName string) (string, []*config.Provider) {
	var names []string
	for name, p := range gw.Providers {
		if p == nil || !p.IsNonChatModel(modelName) {
			continue
		}
		if p.FormatURLs[systemOneURLCacheKey] == "" {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", nil
	}
	// Deterministic order: the caller picks the first, so a Go map iteration
	// must not decide which vendor receives the decision payload.
	sort.Strings(names)
	owners := make([]*config.Provider, 0, len(names))
	for _, name := range names {
		owners = append(owners, gw.Providers[name])
	}
	return names[0], owners
}

// forwardSystemOneRequest performs the retried upstream round-trip and relays
// the non-streaming JSON response, recording usage from its `usage` object.
func (p *Proxy) forwardSystemOneRequest(o forwardOpts) {
	ctxLogger := o.gw.Logger.With("operation", "systemone", "api_key", o.keyRef, "provider", o.providerName)
	// NENYA-363: the dispatch verification pass is deterministic over
	// identical bytes — a block short-circuits before the retry budget is
	// burned (the stale AUD-21 rationale that build failures were
	// "effectively impossible" no longer holds with action=block armed),
	// with the structured refusal instead of the generic network_error.
	// The probe is verdict-only (no side effects): the in-closure pass on
	// the shipping body records the metric/audit exactly once.
	if p.dispatchVerifyBlocked(o.gw, o.bodyBytes) {
		ctxLogger.Warn("dispatch verification blocked the system one request", "model", o.modelName)
		writeStructuredError(o.respWriter, http.StatusForbidden, infra.ErrorKindDispatchBlocked,
			"the request body was refused by the dispatch verification pass")
		return
	}
	resp, err := util.DoWithRetryResp(o.requestCtx, o.maxAttempts, func() (*http.Response, error) {
		// AUD-21 (NENYA-243): every attempt rebuilds the request — the
		// previous Do consumed the body reader, so a re-Do sent an empty
		// request upstream. Genuine build failures still surface as 502
		// (network_error) after retries, matching the embeddings handler;
		// the verification pre-check above already accepted these bytes,
		// so the in-closure pass is a redundant but bounded re-scan.
		req, _, buildErr := p.buildUpstreamRequest(o.gw, o.requestCtx, http.MethodPost, o.endpointURL, o.bodyBytes, o.providerName, o.modelName, "", "", o.srcHeaders)
		if buildErr != nil {
			ctxLogger.Error("failed to create system one upstream request", "err", buildErr)
			return nil, buildErr
		}
		req.Header.Set("Content-Type", "application/json")
		r, fetchErr := o.gw.ClientFor(o.providerName).Do(req)
		if fetchErr != nil {
			if r != nil {
				_ = r.Body.Close()
			}
			return nil, fetchErr
		}
		if r.StatusCode >= 500 {
			_ = r.Body.Close()
			return nil, fmt.Errorf("upstream error: %d", r.StatusCode)
		}
		return r, nil
	})
	if err != nil {
		ctxLogger.Error("system one upstream request failed", "err", err)
		writeStructuredError(o.respWriter, http.StatusBadGateway, infra.ErrorKindNetworkError, "Upstream provider error")
		return
	}
	// Counted on round-trip success (matching the embeddings handler), not
	// at admission — a 502/network-failed dispatch is not an upstream
	// request served.
	o.gw.Metrics.RecordUpstreamRequest(o.modelName, "", o.providerName)
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxNonChatResponseBytes))
	if err != nil {
		ctxLogger.Error("failed to read system one response body", "err", err)
		writeStructuredError(o.respWriter, http.StatusBadGateway, infra.ErrorKindNetworkError, "Failed to read upstream response")
		return
	}

	input, output, usagePresent := systemOneTokenCounts(respBody)
	if usagePresent {
		o.gw.Stats.RecordOutput(o.modelName, output)
	}
	o.gw.Metrics.RecordTokens("input", o.modelName, "", o.providerName, input)
	o.gw.Metrics.RecordTokens("output", o.modelName, "", o.providerName, output)
	// NENYA-336: true-up the TPM bucket on the real input total — the
	// admission debit used the cl100k estimate, which drifts against the
	// provider's own accounting. Rate-limit accounting must not depend on
	// the Metrics instance being present.
	if usagePresent {
		if delta := input - o.estimate; delta > 0 {
			o.gw.RateLimiter.ChargeTPM(o.providerName, o.baseURL, delta)
		}
	}

	writeUpstreamBytesResponse(o.requestCtx, o.respWriter, resp, respBody, ctxLogger)
}

// systemOneTokenCounts extracts the input/output token counts from a System
// One response's usage object. ok is true only when the body parsed AND
// carried a usage object — a valid body without usage reports no counts.
func systemOneTokenCounts(respBody []byte) (input, output int, ok bool) {
	var payload struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil || payload.Usage == nil {
		return 0, 0, false
	}
	return payload.Usage.InputTokens, payload.Usage.OutputTokens, true
}
