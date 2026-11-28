package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/util"
)

func (p *Proxy) handleEmbeddings(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
	keyRef := ""
	if apiKey != nil {
		keyRef = apiKey.Name
	}
	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	bodyBytes, herr := readRequestBody(r, gw, "failed to read embeddings request body")
	if renderBodyReadError(w, herr) {
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		gw.Logger.Warn("failed to parse embeddings JSON", "err", err)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Invalid JSON payload")
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		gw.Logger.Warn("missing or empty model in embeddings request")
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, `Missing or empty "model" field`)
		return
	}
	if len(modelName) > MaxModelNameLength {
		gw.Logger.Warn("model name exceeds maximum length in embeddings request", "length", len(modelName))
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Model name too long")
		return
	}

	matches := routing.ResolveProviders(modelName, gw.Providers, gw.ModelCatalog)
	if len(matches) == 0 {
		gw.Logger.Warn("no provider for embeddings model", "model", modelName)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindModelNotFound, util.ErrNoProvider)
		return
	}

	provider, ok := gw.Providers[matches[0].Provider]
	if !ok {
		gw.Logger.Error("provider not found in providers map", "provider", matches[0].Provider)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindModelNotFound, util.ErrNoProvider)
		return
	}

	// Validate the derived endpoint BEFORE the admission Check: any early
	// return between the Check and dispatch would leak the estimate debit
	// from the TPM bucket (NENYA-336 round-2 review).
	embeddingURL := provider.BaseURL + "/embeddings"
	if embeddingURL == "/embeddings" {
		gw.Logger.Warn("provider BaseURL is empty, cannot derive embeddings endpoint",
			"provider", provider.Name)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Provider does not support embeddings")
		return
	}

	// NENYA-352: the deterministic security subset (secret redaction,
	// entropy redaction, injection detection — chat-parity) scans the
	// input text surfaces BEFORE the token estimate, so the bucket is
	// charged for the text actually dispatched. Rejections and strict
	// faults render here; re-marshaling failures fail closed (a scanned
	// payload that cannot be re-encoded must not dispatch in the clear).
	scannedBody, ok := p.scanEmbeddingsInput(gw, r.Context(), w, payload, bodyBytes)
	if !ok {
		return
	}
	bodyBytes = scannedBody

	// NENYA-336: charge the PRE-DISPATCH INPUT ESTIMATE. String and
	// string-array inputs are countable from the buffered body here;
	// pre-tokenized int-array inputs are uncountable and estimate 0 — the
	// post-dispatch true-up in recordEmbeddingUsage converges the bucket on
	// the real usage totals for those.
	tokenCount := countEmbeddingInputTokens(gw, payload)

	if allowed, rej := gw.RateLimiter.CheckDetailed(provider.Name, provider.BaseURL, tokenCount); !allowed {
		writeRateLimitRejection(gw.Logger, w, provider.Name, rej)
		return
	}

	gw.Stats.RecordRequest(modelName, tokenCount)
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

	p.forwardEmbeddingsRequest(forwardOpts{
		gw: gw, respWriter: w, requestCtx: ctx, endpointURL: embeddingURL, bodyBytes: bodyBytes,
		providerName: provider.Name, baseURL: provider.BaseURL, modelName: modelName, srcHeaders: r.Header,
		maxAttempts: maxAttempts, keyRef: keyRef, estimate: tokenCount,
	})
}

func countEmbeddingInputTokens(gw *gateway.NenyaGateway, payload map[string]interface{}) int {
	inputRaw, ok := payload["input"]
	if !ok {
		return 0
	}

	var totalTokens int
	switch input := inputRaw.(type) {
	case string:
		totalTokens = gw.CountTokens(input)
	case []interface{}:
		for _, item := range input {
			if s, ok := item.(string); ok {
				totalTokens += gw.CountTokens(s)
			}
		}
	}
	return totalTokens
}

func (p *Proxy) forwardEmbeddingsRequest(o forwardOpts) {
	// NENYA-363: the dispatch verification pass is deterministic over
	// identical bytes — a block short-circuits before the retry budget is
	// burned, with the structured refusal instead of a generic 502. The
	// probe is verdict-only (no side effects): the in-closure pass on the
	// shipping body records the metric/audit exactly once.
	if p.dispatchVerifyBlocked(o.gw, o.bodyBytes) {
		ctxLogger := o.gw.Logger.With("operation", "forward", "api_key", o.keyRef, "provider", o.providerName)
		ctxLogger.Warn("dispatch verification blocked the embeddings request", "model", o.modelName)
		writeStructuredError(o.respWriter, http.StatusForbidden, infra.ErrorKindDispatchBlocked,
			"the request body was refused by the dispatch verification pass")
		return
	}
	resp, err := util.DoWithRetryResp(o.requestCtx, o.maxAttempts, func() (*http.Response, error) {
		// AUD-21 (NENYA-243): every attempt rebuilds the request — the
		// previous Do consumed the body reader, so a re-Do sent an empty
		// request upstream (the same defect doUpstreamRoundTrip avoids by
		// building inside the closure). The verification pre-check above
		// already accepted these bytes, so the in-closure pass is a
		// redundant but bounded re-scan.
		req, _, err := p.buildUpstreamRequest(o.gw, o.requestCtx, http.MethodPost, o.endpointURL, o.bodyBytes, o.providerName, o.modelName, "", "", o.srcHeaders)
		if err != nil {
			ctxLogger := o.gw.Logger.With("operation", "forward", "api_key", o.keyRef, "provider", o.providerName)
			ctxLogger.Error("failed to create embeddings upstream request", "err", err)
			return nil, err
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
		ctxLogger := o.gw.Logger.With("operation", "forward", "api_key", o.keyRef, "provider", o.providerName)
		ctxLogger.Error("embeddings upstream request failed", "err", err)
		writeStructuredError(o.respWriter, http.StatusBadGateway, infra.ErrorKindNetworkError, "Upstream provider error")
		return
	}
	// Counted on round-trip success (matching the systemone handler), not
	// at admission — a 502/network-failed dispatch is not an upstream
	// request served.
	o.gw.Metrics.RecordUpstreamRequest(o.modelName, "", o.providerName)
	defer func() { _ = resp.Body.Close() }()

	ctxLogger := o.gw.Logger.With("operation", "forward", "api_key", o.keyRef, "provider", o.providerName)
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxNonChatResponseBytes))
	if err != nil {
		ctxLogger.Error("failed to read embeddings response body", "err", err)
		writeStructuredError(o.respWriter, http.StatusBadGateway, infra.ErrorKindNetworkError, "Failed to read upstream response")
		return
	}

	recordEmbeddingUsage(o.gw, respBody, o.providerName, o.baseURL, o.modelName, o.estimate)

	writeUpstreamBytesResponse(o.requestCtx, o.respWriter, resp, respBody, ctxLogger)
}

func recordEmbeddingUsage(gw *gateway.NenyaGateway, respBody []byte, providerName, baseURL, modelName string, estimate int) {
	var responseMap map[string]interface{}
	if err := json.Unmarshal(respBody, &responseMap); err != nil {
		return
	}
	// modelName (not "") labels every leg so the client − input derivation
	// this metric family exists for stays possible for embeddings traffic.
	inputTokens, _, _ := recordUsageFromMap(gw, responseMap, modelName, providerName)
	gw.Metrics.RecordTokens("input", modelName, "", providerName, inputTokens)
	// NENYA-336: post-dispatch TRUE-UP on the INPUT dimension (the
	// admission debit covers input-only; output tokens were never
	// estimated). The admission check already debited the pre-dispatch
	// input estimate; only the difference to the response's real input
	// usage is charged here — never a refund, so the bucket stays
	// conservatively biased toward the provider's own accounting.
	if delta := inputTokens - estimate; delta > 0 {
		gw.RateLimiter.ChargeTPM(providerName, baseURL, delta)
	}
}
