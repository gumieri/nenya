package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// hopByHopHeaders lists HTTP/1.1 connection-specific headers that must not be
// forwarded by a proxy. Allocated once; not modified after init.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"content-length":      true,
	"upgrade":             true,
	"transfer-encoding":   true,
	"te":                  true,
	"trailers":            true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"keep-alive":          true,
}

// ServeHTTP makes NenyaGateway satisfy the http.Handler interface.
func (g *NenyaGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[PANIC] recovered from panic: %v", rec)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}()

	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	g.handleChatCompletions(w, r)
}

func (g *NenyaGateway) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Client Authentication — must happen before reading the body to avoid
	// letting unauthenticated callers trigger expensive body allocation.
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		log.Printf("[WARN] Missing or malformed Authorization header")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	clientToken := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	// Security: constant-time comparison prevents timing oracle attacks.
	if subtle.ConstantTimeCompare([]byte(clientToken), []byte(g.secrets.ClientToken)) != 1 {
		log.Printf("[WARN] Invalid client token")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Security: Prevent DoS by limiting the request body size.
	r.Body = http.MaxBytesReader(w, r.Body, g.config.Server.MaxBodyBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read request body: %v", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	// Parse the JSON request to extract the model and messages
	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		log.Printf("[WARN] Failed to parse JSON, returning Bad Request.")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// 1. Dynamic Routing: Identify the requested model and build target list.
	modelName, ok := payload["model"].(string)
	if !ok {
		modelName = "glm-5" // Default safe fallback
	}

	// Count tokens from message content only (excludes JSON structure overhead).
	tokenCount := g.countRequestTokens(payload)

	var targets []upstreamTarget
	var cooldownDuration time.Duration

	if agent, ok := g.config.Agents[modelName]; ok {
		secs := agent.CooldownSeconds
		if secs == 0 {
			secs = defaultAgentCooldownSec
		}
		cooldownDuration = time.Duration(secs) * time.Second
		targets = g.buildTargetList(modelName, agent, tokenCount)
		if len(targets) == 0 {
			if len(agent.Models) > 0 {
				log.Printf("[WARN] Agent '%s': all models excluded by max_context (%d tokens)", modelName, tokenCount)
				http.Error(w, "Request too large for all configured models in this agent", http.StatusRequestEntityTooLarge)
			} else {
				log.Printf("[ERROR] Agent '%s': no models configured", modelName)
				http.Error(w, "Agent has no models configured", http.StatusInternalServerError)
			}
			return
		}
		strategy := agent.Strategy
		if strategy == "" {
			strategy = "round-robin"
		}
		log.Printf("[INFO] Agent '%s' (%s): %d model(s) in chain", modelName, strategy, len(targets))
	} else {
		upstreamURL := g.determineUpstream(modelName)
		targets = []upstreamTarget{{url: upstreamURL, model: modelName}}
		log.Printf("[INFO] Model requested: '%s' -> Routing to: %s", modelName, upstreamURL)
	}

	// 2. The Bouncer (Payload Interception) – 3‑Tier Pipeline
	if messagesRaw, ok := payload["messages"]; ok {
		if messages, ok := messagesRaw.([]interface{}); ok && len(messages) > 0 {
			// Tier‑0: Apply regex secret redaction to ALL messages in the
			// conversation history, not just the last one. A secret in an
			// earlier turn would otherwise reach the upstream API unfiltered.
			// Handles both string content and multi-part (array) content.
			anyRedacted := false
			for _, msgRaw := range messages {
				msgNode, ok := msgRaw.(map[string]interface{})
				if !ok {
					continue
				}
				contentRaw, ok := msgNode["content"]
				if !ok {
					continue
				}
				switch content := contentRaw.(type) {
				case string:
					if redacted := g.redactSecrets(content); redacted != content {
						msgNode["content"] = redacted
						anyRedacted = true
					}
				case []interface{}:
					// OpenAI multi-part content: [{"type":"text","text":"..."}]
					for _, partRaw := range content {
						if part, ok := partRaw.(map[string]interface{}); ok {
							if text, ok := part["text"].(string); ok {
								if redacted := g.redactSecrets(text); redacted != text {
									part["text"] = redacted
									anyRedacted = true
								}
							}
						}
					}
				}
			}
			if anyRedacted {
				newBody, err := json.Marshal(payload)
				if err != nil {
					log.Printf("[ERROR] Failed to marshal redacted payload: %v", err)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
					return
				}
				bodyBytes = newBody
			}

			lastMsgRaw := messages[len(messages)-1]
			if lastMsgNode, ok := lastMsgRaw.(map[string]interface{}); ok {
				if contentRaw, ok := lastMsgNode["content"]; ok {
					// Extract text for tier measurement — handles both string and
					// multi-part ([]{"type":"text","text":"…"}) content formats.
					// Multi-part arrays are collapsed to a single string after
					// Tier 2/3 processing (summarization output is always a string).
					var textForInterception string
					switch content := contentRaw.(type) {
					case string:
						textForInterception = content
					case []interface{}:
						var sb strings.Builder
						for _, partRaw := range content {
							if part, ok := partRaw.(map[string]interface{}); ok {
								if t, ok := part["text"].(string); ok {
									sb.WriteString(t)
								}
							}
						}
						textForInterception = sb.String()
					default:
						log.Printf("[WARN] Last message content type unhandled, skipping interception")
					}

					if textForInterception != "" {
						contentRunes := utf8.RuneCountInString(textForInterception)
						softLimit := g.config.Interceptor.SoftLimit
						hardLimit := g.config.Interceptor.HardLimit

						var processed string
						var needsUpdate bool

						// Tier 1: Pass‑through (no intervention)
						if contentRunes < softLimit {
							log.Printf("[INFO] Payload within soft limit (%d runes < %d), passing through", contentRunes, softLimit)
						} else if contentRunes <= hardLimit {
							// Tier 2: Ollama only (The Scalpel)
							log.Printf("[WARN] Payload exceeds soft limit (%d runes), sending to Ollama for summarization...", contentRunes)
							summarized, err := g.summarizeWithOllama(r.Context(), textForInterception)
							if err != nil {
								log.Printf("[ERROR] Ollama summarization failed: %v. Proceeding with original payload.", err)
							} else {
								processed = fmt.Sprintf("[Nenya Sanitized via Ollama]:\n%s", summarized)
								needsUpdate = true
							}
						} else {
							// Tier 3: Truncate + Ollama (The Chainsaw)
							log.Printf("[WARN] Payload exceeds hard limit (%d runes > %d), applying middle‑out truncation before Ollama...", contentRunes, hardLimit)
							truncated := g.truncateMiddleOut(textForInterception, hardLimit)
							summarized, err := g.summarizeWithOllama(r.Context(), truncated)
							if err != nil {
								log.Printf("[ERROR] Ollama summarization failed after truncation: %v. Forwarding truncated payload.", err)
								processed = fmt.Sprintf("[Nenya Truncated (Ollama unreachable)]:\n%s", truncated)
							} else {
								processed = fmt.Sprintf("[Nenya Sanitized via Ollama (truncated input)]:\n%s", summarized)
							}
							needsUpdate = true
						}

						if needsUpdate {
							lastMsgNode["content"] = processed
							newBodyBytes, err := json.Marshal(payload)
							if err != nil {
								log.Printf("[ERROR] Failed to marshal updated payload: %v. Using original payload.", err)
							} else {
								bodyBytes = newBodyBytes
							}
						}
					}
				}
			} else {
				log.Printf("[WARN] Last message is not a map, skipping Ollama interception")
			}
		} else {
			log.Printf("[WARN] Messages field is not a non-empty array, skipping Ollama interception")
		}
	}

	// 3. Forward to upstream with fallback chain (rate limiting checked per-target).
	g.forwardToUpstream(w, r, targets, bodyBytes, cooldownDuration, tokenCount)
}

// summarizeWithOllama handles the local AI inference request to redact/summarize.
// ctx should be the request context so that a client disconnect cancels the
// Ollama call instead of letting it run for the full inference timeout.
func (g *NenyaGateway) summarizeWithOllama(ctx context.Context, heavyText string) (string, error) {
	payload := map[string]interface{}{
		"model":  g.config.Ollama.Model,
		"system": g.config.Ollama.SystemPrompt,
		"prompt": heavyText,
		"stream": false,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal ollama payload: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.config.Ollama.URL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create ollama request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.ollamaClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama unreachable: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(body))
	}

	var ollamaResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return "", fmt.Errorf("failed to decode ollama response: %v", err)
	}

	if summary, ok := ollamaResp["response"].(string); ok {
		return summary, nil
	}
	return "", fmt.Errorf("ollama response missing 'response' field")
}

// forwardToUpstream handles the reverse proxy logic with fallback chain and SSE streaming.
// It tries each target in order, applying per-target rate limiting and retrying on
// 429/5xx with adaptive cooldown.
func (g *NenyaGateway) forwardToUpstream(
	w http.ResponseWriter,
	r *http.Request,
	targets []upstreamTarget,
	body []byte,
	cooldownDuration time.Duration,
	tokenCount int,
) {
	for i, target := range targets {
		// Rate limiting: check (and record) against this specific target's host.
		if !g.checkRateLimit(target.url, tokenCount) {
			log.Printf("[RATELIMIT] Target %d/%d (%s) skipped: rate limit exceeded", i+1, len(targets), target.model)
			continue
		}

		// Set the model for this target and apply provider-specific transformations
		// in a single JSON round-trip.
		transformedBody, finalModel, err := g.transformRequestForUpstream(target.url, body, target.model)
		if err != nil {
			log.Printf("[WARN] Failed to transform request for target %d/%d (%s): %v. Using original body.", i+1, len(targets), target.model, err)
			transformedBody = body
		} else if g.verbose && finalModel != "" {
			log.Printf("[DEBUG] Target %d/%d: using model '%s' for %s", i+1, len(targets), finalModel, target.url)
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, target.url, bytes.NewBuffer(transformedBody))
		if err != nil {
			log.Printf("[ERROR] Failed to create upstream request for target %d/%d: %v", i+1, len(targets), err)
			continue
		}

		// Security: remove client auth header and inject provider key.
		headers := r.Header.Clone()
		headers.Del("Authorization")
		if err := g.injectAPIKey(target.url, target.provider, headers); err != nil {
			log.Printf("[ERROR] Target %d/%d: %v", i+1, len(targets), err)
			http.Error(w, "Gateway configuration error", http.StatusInternalServerError)
			return
		}
		copyHeaders(headers, req.Header)

		if g.verbose {
			debugHeaders := make(http.Header)
			for k, v := range req.Header {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "key") || strings.Contains(lk, "auth") {
					debugHeaders[k] = []string{"[REDACTED]"}
				} else {
					debugHeaders[k] = v
				}
			}
			log.Printf("[DEBUG] Forwarding to upstream: %s (target %d/%d)", target.url, i+1, len(targets))
			log.Printf("[DEBUG] Request headers: %v", debugHeaders)
			if len(transformedBody) > 0 && len(transformedBody) < 1000 {
				log.Printf("[DEBUG] Request body: %s", string(transformedBody))
			}
		}

		resp, err := g.client.Do(req)
		if err != nil {
			log.Printf("[WARN] Target %d/%d (%s) network error: %v", i+1, len(targets), target.model, err)
			continue
		}

		log.Printf("[INFO] Target %d/%d (%s) response: %d", i+1, len(targets), target.model, resp.StatusCode)

		// Retryable: 429 rate-limited or 5xx server errors → try next target.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
			resp.Body.Close()
			log.Printf("[WARN] Target %d/%d (%s) returned %d — activating cooldown and trying next target",
				i+1, len(targets), target.model, resp.StatusCode)
			if g.verbose && len(errorBody) > 0 {
				log.Printf("[DEBUG] Error body: %s", string(errorBody))
			}
			if target.coolKey != "" && cooldownDuration > 0 {
				g.agentMu.Lock()
				g.modelCooldowns[target.coolKey] = time.Now().Add(cooldownDuration)
				g.agentMu.Unlock()
			}
			continue
		}

		// Non-retryable 4xx — return error immediately without trying other targets.
		if resp.StatusCode >= 400 {
			errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
			resp.Body.Close()
			if len(errorBody) > 0 {
				log.Printf("[ERROR] Target %d/%d (%s) returned %d: %s", i+1, len(targets), target.model, resp.StatusCode, string(errorBody))
			} else {
				log.Printf("[ERROR] Target %d/%d (%s) returned %d: empty body", i+1, len(targets), target.model, resp.StatusCode)
			}
			http.Error(w, "Upstream provider error", resp.StatusCode)
			return
		}

		// Success — stream response to client.
		copyHeaders(resp.Header, w.Header())
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Printf("[DEBUG] Stream copy ended: %v", err)
		}
		resp.Body.Close()
		return
	}

	// All targets exhausted or failed.
	log.Printf("[ERROR] All %d upstream target(s) exhausted", len(targets))
	http.Error(w, "All upstream targets exhausted", http.StatusServiceUnavailable)
}

// copyHeaders copies src headers to dst, excluding hop-by-hop headers.
func copyHeaders(src, dst http.Header) {
	for k, vv := range src {
		if hopByHopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
