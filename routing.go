package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// geminiModelMap translates AI Studio UI names and shorthand aliases to the actual
// Gemini API model IDs. Only entries that require renaming are listed; unknown names
// pass through unchanged so newly released models work without a gateway update.
// Allocated once at startup; not modified after init.
var geminiModelMap = map[string]string{
	// Gemini 3 preview — API requires the -preview suffix
	"gemini-3-flash": "gemini-3-flash-preview",
	"gemini-3-pro":   "gemini-3-pro-preview",

	// Gemini 3.1 preview — same convention
	"gemini-3.1-flash":      "gemini-3.1-flash-preview",
	"gemini-3.1-flash-lite": "gemini-3.1-flash-lite-preview",
	"gemini-3.1-pro":        "gemini-3.1-pro-preview",

	// Unversioned aliases → latest stable generation
	"gemini-flash":      "gemini-2.5-flash",
	"gemini-flash-lite": "gemini-2.5-flash-lite",
	"gemini-pro":        "gemini-2.5-pro",
}

// upstreamTarget is one entry in a resolved request's fallback chain.
type upstreamTarget struct {
	url      string // upstream endpoint URL
	model    string // model name to send to this upstream
	coolKey  string // "provider:model" key used for cooldown tracking (empty for non-agent)
	provider string // provider name ("gemini", "ollama", …) used for API key injection
}

// determineUpstream dynamically routes the request based on the model name.
// Relies on loadConfig having populated all UpstreamConfig URLs with defaults.
func (g *NenyaGateway) determineUpstream(modelName string) string {
	modelName = strings.ToLower(modelName)
	switch {
	case strings.HasPrefix(modelName, "gemini-"):
		return g.config.Upstream.GeminiURL
	case strings.HasPrefix(modelName, "deepseek-"):
		return g.config.Upstream.DeepSeekURL
	case strings.HasPrefix(modelName, "llama-"),
		strings.HasPrefix(modelName, "llama3-"),
		strings.HasPrefix(modelName, "mixtral-"),
		strings.HasPrefix(modelName, "whisper-"):
		return g.config.Upstream.GroqURL
	case strings.HasPrefix(modelName, "meta-llama/"),
		strings.HasPrefix(modelName, "mistralai/"),
		strings.HasPrefix(modelName, "qwen/"),
		strings.HasPrefix(modelName, "together/"):
		return g.config.Upstream.TogetherURL
	default:
		return g.config.Upstream.ZaiURL
	}
}

// providerURL resolves a provider name to an upstream endpoint URL.
// agentURL overrides the default when set (per-entry URL in agent config).
// Relies on loadConfig having populated all UpstreamConfig URLs with defaults.
func (g *NenyaGateway) providerURL(provider, agentURL string) string {
	if agentURL != "" {
		return agentURL
	}
	switch provider {
	case "gemini":
		return g.config.Upstream.GeminiURL
	case "deepseek":
		return g.config.Upstream.DeepSeekURL
	case "groq":
		return g.config.Upstream.GroqURL
	case "together":
		return g.config.Upstream.TogetherURL
	case "ollama":
		return defaultOllamaOpenAIURL
	default: // "zai" and anything else
		return g.config.Upstream.ZaiURL
	}
}

// buildTargetList produces an ordered list of targets to try for the given agent.
//
// Strategy:
//   - "round-robin" (default): rotates the starting position each request to
//     spread load; falls back to the next model on failure.
//   - "fallback": always starts from index 0; advances only on failure.
//
// Filtering:
//   - Models with max_context > 0 and tokenCount > max_context are excluded
//     entirely — they are not placed in the cooling fallback list.
//   - Models in cooldown are appended after active models as last resort.
func (g *NenyaGateway) buildTargetList(agentName string, agent AgentConfig, tokenCount int) []upstreamTarget {
	n := len(agent.Models)
	if n == 0 {
		return nil
	}

	g.agentMu.Lock()
	var start int
	if strings.ToLower(agent.Strategy) != "fallback" {
		// Round-robin (default): advance counter to rotate starting position.
		start = int(g.agentCounters[agentName]) % n
		g.agentCounters[agentName]++
	}
	// Fallback strategy: start = 0 (zero value) — always try from the beginning.
	now := time.Now()
	cooldowns := make(map[string]time.Time, len(g.modelCooldowns))
	for k, v := range g.modelCooldowns {
		cooldowns[k] = v
	}
	g.agentMu.Unlock()

	active := make([]upstreamTarget, 0, n)
	cooling := make([]upstreamTarget, 0, n)

	for i := 0; i < n; i++ {
		m := agent.Models[(start+i)%n]

		// Skip models that cannot handle the request's context size.
		if m.MaxContext > 0 && tokenCount > m.MaxContext {
			log.Printf("[INFO] Skipping model %s (max_context=%d < request=%d tokens)",
				m.Model, m.MaxContext, tokenCount)
			continue
		}

		t := upstreamTarget{
			url:      g.providerURL(m.Provider, m.URL),
			model:    m.Model,
			coolKey:  m.Provider + ":" + m.Model,
			provider: m.Provider,
		}
		if now.Before(cooldowns[t.coolKey]) {
			cooling = append(cooling, t)
		} else {
			active = append(active, t)
		}
	}
	return append(active, cooling...)
}

// transformRequestForUpstream sets the model field in the request body (when model is
// non-empty) and applies any provider-specific transformations (e.g. Gemini model
// name mapping). Returns the transformed body, the final model name, and any error.
// Passing model="" leaves the model field in the body unchanged.
func (g *NenyaGateway) transformRequestForUpstream(upstreamURL string, body []byte, model string) ([]byte, string, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", fmt.Errorf("failed to parse request body for transformation: %v", err)
	}

	// Override model field if a specific model was requested for this target.
	if model != "" {
		payload["model"] = model
	}

	modelRaw, ok := payload["model"]
	if !ok {
		return body, "", nil // No model field, nothing to transform
	}

	modelName, ok := modelRaw.(string)
	if !ok {
		return body, "", nil // Model is not a string
	}

	finalModel := modelName

	// Gemini model mapping: translate UI names / aliases to the actual API model IDs.
	// Unknown names pass through unchanged so newly released models work without a
	// gateway update. geminiModelMap is a package-level var allocated once at startup.
	if strings.Contains(upstreamURL, "generativelanguage.googleapis.com") {
		if mapped, ok := geminiModelMap[strings.ToLower(modelName)]; ok {
			finalModel = mapped
		}
		payload["model"] = finalModel
		if finalModel != modelName {
			log.Printf("[INFO] Gemini model mapping: %s -> %s", modelName, finalModel)
		}
	}

	newBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal transformed request: %v", err)
	}

	return newBody, finalModel, nil
}

// injectAPIKey sets the appropriate Authorization header(s) for the given upstream URL.
// provider is the agent model's provider name (e.g. "ollama") and is checked first so
// that custom URL overrides for Ollama work regardless of the host. Returns an error if
// the required key is missing or the URL is unrecognised; the caller writes the error.
func (g *NenyaGateway) injectAPIKey(upstreamURL, provider string, headers http.Header) error {
	if provider == "ollama" {
		return nil // no API key required for local inference
	}
	switch {
	case strings.Contains(upstreamURL, "generativelanguage.googleapis.com"):
		if g.secrets.GeminiKey == "" {
			return fmt.Errorf("Gemini API key missing in secrets")
		}
		headers.Set("Authorization", "Bearer "+g.secrets.GeminiKey)
		headers.Set("x-goog-api-key", g.secrets.GeminiKey)
	case strings.Contains(upstreamURL, "api.deepseek.com"):
		if g.secrets.DeepSeekKey == "" {
			return fmt.Errorf("DeepSeek API key missing in secrets")
		}
		headers.Set("Authorization", "Bearer "+g.secrets.DeepSeekKey)
	case strings.Contains(upstreamURL, "api.z.ai"):
		if g.secrets.ZaiKey == "" {
			return fmt.Errorf("z.ai API key missing in secrets")
		}
		headers.Set("Authorization", "Bearer "+g.secrets.ZaiKey)
	case strings.Contains(upstreamURL, "api.groq.com"):
		if g.secrets.GroqKey == "" {
			return fmt.Errorf("Groq API key missing in secrets")
		}
		headers.Set("Authorization", "Bearer "+g.secrets.GroqKey)
	case strings.Contains(upstreamURL, "api.together.xyz"):
		if g.secrets.TogetherKey == "" {
			return fmt.Errorf("Together AI API key missing in secrets")
		}
		headers.Set("Authorization", "Bearer "+g.secrets.TogetherKey)
	default:
		return fmt.Errorf("unrecognised upstream URL: %s", upstreamURL)
	}
	return nil
}
