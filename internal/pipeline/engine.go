package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/util"
)

// MaxOllamaResponseBytes is the maximum response size accepted from the
// local engine (Ollama) to prevent memory exhaustion.
const MaxOllamaResponseBytes = 512 * 1024

// MaxErrorBodyBytes is the maximum number of bytes read from upstream
// error response bodies for logging/classification.
const MaxErrorBodyBytes = 8 * 1024

// System One transport constants. The state (adjudicated excerpt) is
// capped at 512 tokens ≈ 2048 bytes (the typed-decision models' state
// window; Laya/Jev documented limit), and answers ride a typed JSON
// envelope — no prose parsing on this transport.
const (
	// SystemOneStateCapBytes caps the state field of a System One
	// decision request (512-token state window, ~4 bytes/token).
	SystemOneStateCapBytes = 2048
	// SystemOneAPIFormat is the provider ApiFormat value selecting the
	// System One transport for judgment targets.
	SystemOneAPIFormat = "systemone"
)

// ErrSystemOneLowConfidence is returned by the System One transport
// when the typed answer's confidence is below the question's
// escalate_below_confidence threshold: the chain falls through to the
// next target (confidence routes, never decides policy).
var ErrSystemOneLowConfidence = errors.New("system one: answer confidence below escalate threshold")

// systemOneQuestion is the contract-derived decision question sent to
// System One targets (choice type covers the 2..~20 verdict enums;
// binary enums may alternatively be served as noul by the sidecar).
type systemOneQuestion struct {
	// ID is the question/answer correlation key.
	ID string
	// Prompt carries the contract's system prompt (the decision
	// criteria).
	Prompt string
	// Choices is the contract's closed verdict enum.
	Choices []string
	// ConfidenceBelow (0 = off) makes the transport refuse answers
	// with confidence below it, cascading to the next chain target.
	ConfidenceBelow float64
	// state carries the adjudicated excerpt (capped in transport).
	state string
}

// CallEngine sends a prompt to the local engine (e.g. Ollama) for
// summarization or redaction. It handles both OpenAI and Ollama API formats.
func CallEngine(ctx context.Context, httpClient *http.Client, provider *config.Provider, engine config.EngineConfig, injectAPIKey func(providerName string, headers http.Header) error, systemPrompt, prompt string) (string, error) {
	apiFormat := provider.ApiFormat
	if apiFormat == "" {
		apiFormat = "openai"
	}

	var payload map[string]interface{}
	switch apiFormat {
	case "ollama":
		payload = map[string]interface{}{
			"model":  engine.Model,
			"system": systemPrompt,
			"prompt": prompt,
			"stream": false,
		}
	default:
		payload = map[string]interface{}{
			"model": engine.Model,
			"messages": []map[string]string{
				{"role": "system", "content": systemPrompt},
				{"role": "user", "content": prompt},
			},
			"stream": false,
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal engine payload: %v", err)
	}

	maxRetries := provider.MaxRetryAttempts
	if maxRetries <= 0 {
		maxRetries = util.DefaultMaxRetryAttempts()
	}

	resp, err := util.DoWithRetryResp(ctx, maxRetries, func() (*http.Response, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, bytes.NewBuffer(encoded))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Content-Type", "application/json")

		if authErr := injectAPIKey(engine.Provider, req.Header); authErr != nil {
			return nil, fmt.Errorf("engine auth failed: %v", authErr)
		}

		r, doErr := httpClient.Do(req)
		if doErr != nil {
			if r != nil {
				_ = r.Body.Close()
			}
			return nil, doErr
		}
		if r.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(r.Body, MaxErrorBodyBytes))
			_ = r.Body.Close()
			statusErr := fmt.Errorf("engine returned status %d: %s", r.StatusCode, string(body))
			// AGENTS.md §8: only 429 and 5xx are retryable; deterministic
			// 4xx (bad model, auth, bad request) fail the target outright.
			if r.StatusCode != http.StatusTooManyRequests && r.StatusCode < 500 {
				return nil, &util.PermanentError{Err: statusErr}
			}
			return nil, statusErr
		}
		return r, nil
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var response map[string]interface{}
	if decodeErr := json.NewDecoder(io.LimitReader(resp.Body, MaxOllamaResponseBytes)).Decode(&response); decodeErr != nil {
		return "", fmt.Errorf("failed to decode engine response (capped at %d bytes): %w", MaxOllamaResponseBytes, decodeErr)
	}

	output, err := extractEngineOutput(response, apiFormat)
	if err != nil {
		return "", err
	}
	return output, nil
}

func extractEngineOutput(response map[string]interface{}, apiFormat string) (string, error) {
	switch apiFormat {
	case "ollama":
		return extractOllamaOutput(response)
	default:
		return extractOpenAIOutput(response)
	}
}

func extractTextFromParts(parts []interface{}) (string, bool) {
	for _, part := range parts {
		p, ok := part.(map[string]interface{})
		if !ok || p["type"] != "text" {
			continue
		}
		if text, ok := p["text"].(string); ok {
			return text, true
		}
	}
	return "", false
}

func extractOllamaOutput(response map[string]interface{}) (string, error) {
	output, ok := response["response"].(string)
	if !ok {
		return "", errors.New("engine response missing 'response' field")
	}
	return output, nil
}

func extractOpenAIOutput(response map[string]interface{}) (string, error) {
	choices, ok := response["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", errors.New("openai response missing choices")
	}

	choice, cok := choices[0].(map[string]interface{})
	if !cok {
		return "", errors.New("openai choice is not an object")
	}

	msg, mok := choice["message"].(map[string]interface{})
	if !mok {
		return "", errors.New("openai choice missing message")
	}

	if contentStr, cok := msg["content"].(string); cok {
		return contentStr, nil
	}

	parts, pok := msg["content"].([]interface{})
	if !pok {
		return "", errors.New("openai message content has unsupported type")
	}

	if text, found := extractTextFromParts(parts); found {
		return text, nil
	}

	return "", errors.New("openai content parts carry no text")
}

// ClientResolver returns the HTTP client for dispatching engine requests to
// the named provider (e.g. gateway.NenyaGateway.ClientFor). This lets engine
// targets honor per-provider response-header timeouts.
type ClientResolver func(providerName string) *http.Client

// EngineCallObserver observes one engine-chain attempt. provider is
// the target's provider name; err is nil for a successful attempt.
// Observers run synchronously on the calling goroutine after each
// attempt completes; a resolver failure (nil client) also emits an
// event, with a zero duration since no HTTP attempt was made.
type EngineCallObserver func(attempt, total int, provider string, err error, duration time.Duration)

// EngineChainCall groups the parameters of an engine-chain invocation
// (AGENTS.md §11 parameter grouping).
type EngineChainCall struct {
	// Caller labels the invocation site in logs: "judgment_<name>" is
	// the default form; contracts may pin a legacy label via
	// JudgmentContract.Caller (e.g. "injection_escalation").
	Caller string
	// AgentName is the agent whose request triggered the call.
	AgentName string
	// System is the system prompt for the engine.
	System string
	// Prompt is the user prompt for the engine.
	Prompt string
	// Observer optionally receives one event per attempt; nil disables
	// observation.
	Observer EngineCallObserver
	// systemOne, when non-nil, routes targets whose provider ApiFormat
	// is "systemone" through the typed System One transport instead of
	// the chat formats (chat targets in the same chain keep the chat
	// transport — the cascade path).
	systemOne *systemOneQuestion
}

// CallEngineChain tries each engine target in order, returning the first
// successful summarization. Each target gets its own timeout; failures log a
// warning and fall through to the next target.
func CallEngineChain(ctx context.Context, clientFor ClientResolver,
	targets []config.EngineTarget, logger *slog.Logger,
	injectAPIKey func(providerName string, headers http.Header) error,
	caller, agentName, systemPrompt, prompt string) (string, error) {
	return CallEngineChainObserved(ctx, clientFor, targets, logger, injectAPIKey, EngineChainCall{
		Caller:    caller,
		AgentName: agentName,
		System:    systemPrompt,
		Prompt:    prompt,
	})
}

// CallEngineChainObserved is CallEngineChain with an optional observer
// receiving one event per attempt (see EngineChainCall.Observer).
func CallEngineChainObserved(ctx context.Context, clientFor ClientResolver,
	targets []config.EngineTarget, logger *slog.Logger,
	injectAPIKey func(providerName string, headers http.Header) error,
	call EngineChainCall) (string, error) {
	if len(targets) == 0 {
		return "", errors.New("engine chain: no targets available")
	}
	if clientFor == nil {
		return "", errors.New("engine chain: no client resolver configured (refusing http.DefaultClient)")
	}

	var lastErr error
	for i, target := range targets {
		attempt := i + 1
		total := len(targets)

		logger.Info("engine call attempt",
			"caller", call.Caller,
			"agent", call.AgentName,
			"provider", target.Provider.Name,
			"model", target.Engine.Model,
			"attempt", attempt,
			"total", total)

		client := clientFor(target.Provider.Name)
		if client == nil {
			err := fmt.Errorf("engine chain: client resolver returned nil client for provider %q", target.Provider.Name)
			if call.Observer != nil {
				call.Observer(attempt, total, target.Provider.Name, err, 0)
			}
			lastErr = err
			logger.Warn("engine call failed",
				"caller", call.Caller,
				"agent", call.AgentName,
				"provider", target.Provider.Name,
				"model", target.Engine.Model,
				"attempt", attempt,
				"total", total,
				"err", err)
			continue
		}

		timeout := target.Engine.TimeoutSeconds
		if timeout <= 0 {
			timeout = 60
		}
		engineCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		start := time.Now()
		var result string
		var err error
		if target.Provider.ApiFormat == SystemOneAPIFormat {
			if call.systemOne == nil {
				err = errors.New("system one target unsupported for this judgment contract")
			} else {
				result, err = callEngineSystemOne(engineCtx, client, target.Provider, target.Engine, injectAPIKey, call.systemOne)
			}
		} else {
			result, err = CallEngine(engineCtx, client, target.Provider, target.Engine, injectAPIKey, call.System, call.Prompt)
		}
		duration := time.Since(start)
		cancel()

		if call.Observer != nil {
			call.Observer(attempt, total, target.Provider.Name, err, duration)
		}

		if err != nil {
			lastErr = err
			logger.Warn("engine call failed",
				"caller", call.Caller,
				"agent", call.AgentName,
				"provider", target.Provider.Name,
				"model", target.Engine.Model,
				"attempt", attempt,
				"total", total,
				"err", err)
			continue
		}

		logger.Info("engine call success",
			"caller", call.Caller,
			"agent", call.AgentName,
			"provider", target.Provider.Name,
			"model", target.Engine.Model,
			"attempt", attempt,
			"total", total)
		return result, nil
	}

	return "", fmt.Errorf("engine chain failed after %d attempts: last error: %w", len(targets), lastErr)
}
