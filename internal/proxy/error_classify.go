package proxy

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
)

// derive429Cooldown merges the configured cooldown with upstream-declared
// windows (NENYA-43), longest wins: body quota patterns, flexible body reset
// fields, and header windows (Retry-After / Anthropic unified resets /
// x-ratelimit-reset). Parsing is 429-gated to preserve existing semantics —
// 5xx errors keep serverCooldown regardless of body wording. Returns the
// cooldown and whether a quota signal was seen.
func derive429Cooldown(logger *slog.Logger, errorBody []byte, header http.Header, status int, cooldownDuration time.Duration) (time.Duration, bool) {
	if status != http.StatusTooManyRequests {
		return cooldownDuration, false
	}
	effective, isQuota := mergeBodyQuotaSignals(logger, errorBody, cooldownDuration)
	if upstreamCD := deriveUpstreamRateLimitCooldown(header); upstreamCD > effective {
		effective = upstreamCD
		logger.Info("upstream rate-limit window cooldown derived", "cooldown_s", effective.Seconds())
	}
	return effective, isQuota
}

// mergeBodyQuotaSignals folds body-side quota signals into the effective
// cooldown. Detection (isQuota) is decoupled from extension: a quota signal
// below the configured cooldown still flags quota exhaustion for the
// error-kind contract and the NENYA-41 floor.
func mergeBodyQuotaSignals(logger *slog.Logger, errorBody []byte, effective time.Duration) (time.Duration, bool) {
	if len(errorBody) == 0 {
		return effective, false
	}
	isQuota := false
	if quotaCD := parseQuotaExhaustion(errorBody, logger); quotaCD > 0 {
		isQuota = true
		effective = extendForCooldownSignal(logger, effective, quotaCD, "quota exhaustion detected, extending cooldown")
	}
	if resetCD := parseQuotaResetFields(errorBody); resetCD > 0 {
		isQuota = true
		effective = extendForCooldownSignal(logger, effective, resetCD, "quota reset field detected, extending cooldown")
	}
	return effective, isQuota
}

// extendForCooldownSignal returns the longer of the current effective
// cooldown and a detected signal, logging when the signal wins.
func extendForCooldownSignal(logger *slog.Logger, effective, signal time.Duration, reason string) time.Duration {
	if signal <= effective {
		return effective
	}
	logger.Info(reason, "cooldown_s", signal.Seconds())
	return signal
}

func handleRetryableError429(logger *slog.Logger, errorBody []byte, action upstreamAction, cooldownDuration time.Duration, target routing.UpstreamTarget, agentName string, gw *gateway.NenyaGateway) (time.Duration, bool) {
	effectiveCooldown, isQuota := derive429Cooldown(logger, errorBody, action.resp.Header, action.resp.StatusCode, cooldownDuration)

	if action.resp.StatusCode == http.StatusTooManyRequests {
		// Quota floor (NENYA-41): never let a sub-second provider-supplied
		// cooldown produce a zero-wait retry storm against the same account.
		if floor := gw.Config.Governance.EffectiveMinQuotaCooldown(); isQuota && floor > 0 && effectiveCooldown < floor {
			effectiveCooldown = floor
			logger.Info("quota cooldown floored", "cooldown_s", effectiveCooldown.Seconds())
		}
		// Idle-connection eviction (NENYA-47): an exhausted account's pooled
		// connections are dead weight — drop them while the account cools
		// down. Called without gateway locks held.
		gw.EvictIdleConnections(target.Provider)
		gw.AgentState.ActivateCooldown(target, effectiveCooldown)
		gw.Metrics.RecordCooldown(agentName, target.Provider, target.Model)
		gw.AgentState.RecordFailureWithStatus(target, action.resp.StatusCode, string(errorBody))
		return parseRetryDelay(action.resp.Header, errorBody), isQuota
	}

	gw.AgentState.RecordFailureWithStatus(target, action.resp.StatusCode, string(errorBody))
	return 0, false
}

// parseQuotaExhaustion extracts quota cooldown duration from error responses.
// It checks for ZAI-specific error codes (1308, 1310) with Unix millisecond
// timestamps in the message, and falls back to generic quota pattern matching.
// Returns 0 if no quota information is found.
func parseQuotaExhaustion(body []byte, logger *slog.Logger) time.Duration {
	if len(body) == 0 {
		return 0
	}

	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if json.Unmarshal(body, &errResp) != nil {
		return parseGenericQuotaPatterns(body)
	}

	if errResp.Error.Code == "" {
		return parseGenericQuotaPatterns(body)
	}

	switch errResp.Error.Code {
	case "1308":
		return parseZaiQuota1308(errResp.Error.Message)
	case "1310":
		return parseZaiQuota1310(errResp.Error.Message)
	default:
		return parseGenericQuotaPatterns(body)
	}
}

func parseZaiQuota1308(message string) time.Duration {
	ts := extractUnixTimestampMs(message)
	if ts <= 0 {
		return zaiCode1308FallbackCooldown
	}

	dur := time.Until(time.UnixMilli(ts))
	if dur > 0 {
		return dur
	}
	return zaiCode1308FallbackCooldown
}

func parseZaiQuota1310(message string) time.Duration {
	ts := extractUnixTimestampMs(message)
	if ts <= 0 {
		return zaiCode1310FallbackCooldown
	}

	dur := time.Until(time.UnixMilli(ts))
	if dur <= 0 {
		return zaiCode1310FallbackCooldown
	}

	if dur < zaiCode1310MinCooldown {
		return zaiCode1310MinCooldown
	}
	if dur > maxQuotaCooldown {
		return maxQuotaCooldown
	}
	return dur
}

func parseGenericQuotaPatterns(body []byte) time.Duration {
	lower := strings.ToLower(string(body))

	if strings.Contains(lower, "per 86400s") || strings.Contains(lower, "perday") {
		return maxQuotaCooldown
	}

	quotaPatterns := []string{
		"resource_exhausted",
		"quota exceeded",
		"quota_exceeded",
	}
	for _, p := range quotaPatterns {
		if strings.Contains(lower, p) {
			return 5 * time.Minute
		}
	}

	return 0
}

// extractUnixTimestampMs searches for and extracts the first 13+ digit
// number from the message, returning it as a millisecond-precision Unix timestamp.
// Only timestamps between 2023-01-01 (1672531200000) and 9999-12-31 23:59:59 UTC
// (410244479903999) are accepted — numbers outside this range are rejected.
// Returns 0 if no 13+ digit number is found, or if the number is outside the valid range.
// This function is stateless and safe for concurrent use.
func extractUnixTimestampMs(msg string) int64 {
	const (
		minTimestamp = int64(1672531200000)
		maxTimestamp = int64(410244479903999)
	)

	for i := 0; i < len(msg); i++ {
		if msg[i] < '0' || msg[i] > '9' {
			continue
		}

		j := i
		for j < len(msg) && msg[j] >= '0' && msg[j] <= '9' {
			j++
		}

		if j-i < 13 {
			i = j
			continue
		}

		ts := parseDigitsToTimestamp(msg[i:j], minTimestamp, maxTimestamp)
		if ts > 0 {
			return ts
		}
		i = j
	}
	return 0
}

// parseDigitsToTimestamp parses a digit string into an int64 timestamp,
// applying overflow protection and range validation.
func parseDigitsToTimestamp(digits string, minTimestamp, maxTimestamp int64) int64 {
	var ts int64
	for k := range len(digits) {
		digit := int64(digits[k] - '0')
		if ts > (math.MaxInt64-digit)/10 {
			return 0
		}
		ts = ts*10 + digit
	}

	if ts < minTimestamp || ts > maxTimestamp {
		return 0
	}
	return ts
}

// matchRequestScopedError returns the provider-configured
// request_scoped_errors rule the upstream error matches (NENYA-42): the
// client's payload is at fault regardless of the status class. Rules with a
// nonzero Status apply only to that status; rules with an empty
// MessagePattern never match. Returns nil when no rule matches.
func (p *Proxy) matchRequestScopedError(gw *gateway.NenyaGateway, providerName string, statusCode int, body []byte) *config.RequestScopedErrorRule {
	pr, ok := gw.Providers[providerName]
	if !ok || len(pr.RequestScopedErrors) == 0 {
		return nil
	}
	lowerBody := strings.ToLower(string(body))
	for i := range pr.RequestScopedErrors {
		rule := &pr.RequestScopedErrors[i]
		if rule.Status != 0 && rule.Status != statusCode {
			continue
		}
		pattern := strings.TrimSpace(rule.MessagePattern)
		if pattern == "" {
			continue
		}
		if strings.Contains(lowerBody, strings.ToLower(pattern)) {
			return rule
		}
	}
	return nil
}

func matchProviderSpecificPatterns(lowerBody string, provider string) bool {
	lp := strings.ToLower(provider)
	for _, m := range providerMatchers {
		if strings.Contains(lp, m.name) {
			for _, pat := range m.patterns {
				if strings.Contains(lowerBody, pat) {
					return true
				}
			}
		}
	}
	return false
}

func isRetryableClientErrorForProvider(statusCode int, body []byte, provider string) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusRequestEntityTooLarge && statusCode != http.StatusUnprocessableEntity {
		return false
	}
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))

	for _, pat := range upstreamBlamePatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}

	for _, pat := range commonRetryablePatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}

	return matchProviderSpecificPatterns(lower, provider)
}

// errorEnvelopeKeys are the JSON object keys that identify a provider error
// envelope. A 4xx body carrying any of them is a deliberate, structured
// client error; a JSON object with none of them is treated as an opaque
// relayed failure (see opaque4xxBody).
var errorEnvelopeKeys = []string{
	"error", "errors", "detail", "details", "message", "type", "code", "title", "reason", "status",
}

// opaque4xxBody reports whether a 400/422 response body is a non-empty JSON
// object carrying none of the recognized error-envelope keys — the shape
// aggregators and gateways produce when they relay an upstream failure without
// wrapping it (e.g. `{"model":"..."}`). Empty, empty-object, non-JSON,
// non-object, and enveloped bodies all return false, so genuine client errors
// stay non-retryable.
//
// 413 (Request Entity Too Large) is deliberately excluded: the payload is an
// immutable part of an identical retry, so both a same-target repeat and a
// failover would re-send the same oversized body (possibly leaking it to
// another provider) and fail again.
func opaque4xxBody(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
	default:
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || len(obj) == 0 {
		return false
	}
	for _, key := range errorEnvelopeKeys {
		if _, ok := obj[key]; ok {
			return false
		}
	}
	return true
}

// retryable4xxReason classifies why a 4xx body is retryable (empty = not).
type retryable4xxReason string

const (
	retryable4xxNone    retryable4xxReason = ""
	retryable4xxPattern retryable4xxReason = "pattern"
	retryable4xxPhrase  retryable4xxReason = "phrase"
	retryable4xxOpaque  retryable4xxReason = "opaque"
)

// retryable4xxReason reports the classifier verdict for a 4xx body, along with
// whether the failure is safe to re-send unchanged to the *same* target. A 413
// whose body matches a context/max_tokens pattern is retryable (another target
// may accept the payload), but repeating it in place can only fail again, so
// callers must fail over rather than same-target retry.
func retryable4xxReasonFor(gw *gateway.NenyaGateway, statusCode int, body []byte, provider string) retryable4xxReason {
	if isRetryableClientErrorForProvider(statusCode, body, provider) {
		return retryable4xxPattern
	}
	if provider != "" && gw != nil {
		if pr, ok := gw.Providers[provider]; ok && pr != nil && statusCode >= 400 && statusCode < 500 &&
			len(pr.RetryablePhrases) > 0 && bodyContainsAnyPhrases(body, pr.RetryablePhrases) {
			return retryable4xxPhrase
		}
	}
	if gw != nil && gw.Config.Governance.Opaque4xxRetryEnabled() && opaque4xxBody(statusCode, body) {
		return retryable4xxOpaque
	}
	return retryable4xxNone
}

// bodyContainsAnyPhrases reports whether body contains any of the given
// phrases, case-insensitively. Used for provider-configured
// retryable_phrases.
func bodyContainsAnyPhrases(body []byte, phrases []string) bool {
	if len(body) == 0 || len(phrases) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, phrase := range phrases {
		if strings.TrimSpace(phrase) == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(phrase)) {
			return true
		}
	}
	return false
}

type rpcDetail struct {
	RetryDelay string `json:"retryDelay"`
	Type       string `json:"@type"`
}

func capRetryDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > maxRetryBackoff {
		return maxRetryBackoff
	}
	return d
}
