package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tiktoken "github.com/pkoukk/tiktoken-go"
	toml "github.com/pelletier/go-toml/v2"
)

var _ = context.TODO

// Config holds the environment and core configurations.
type Config struct {
	Server      ServerConfig
	Interceptor InterceptorConfig
	Ollama      OllamaConfig
	RateLimit   RateLimitConfig `toml:"ratelimit"`
}

type ServerConfig struct {
	ListenAddr   string `toml:"listen_addr"`
	MaxBodyBytes int64  `toml:"max_body_bytes"`
}

type InterceptorConfig struct {
	SoftLimit          int     `toml:"soft_limit"`           // characters (runes)
	HardLimit          int     `toml:"hard_limit"`           // characters (runes)
	TruncationStrategy string  `toml:"truncation_strategy"`  // "middle-out"
	KeepFirstPercent   float64 `toml:"keep_first_percent"`   // e.g., 15.0
	KeepLastPercent    float64 `toml:"keep_last_percent"`    // e.g., 25.0
}

type RateLimitConfig struct {
	MaxTPM int `toml:"max_tpm"`  // tokens per minute
	MaxRPM int `toml:"max_rpm"`  // requests per minute
}

type SecretsConfig struct {
	ClientToken  string `json:"client_token"`   // auth for clients
	GeminiKey    string `json:"gemini_key"`     // Gemini API key
	DeepSeekKey  string `json:"deepseek_key"`   // DeepSeek API key  
	ZaiKey       string `json:"zai_key"`        // z.ai API key
}

type OllamaConfig struct {
	URL          string `toml:"url"`
	Model        string `toml:"model"`
	SystemPrompt string `toml:"system_prompt"`
}

// loadConfig reads and parses a TOML configuration file.
func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %v", filename, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %v", filename, err)
	}

	// Validate required fields
	if cfg.Server.ListenAddr == "" {
		cfg.Server.ListenAddr = ":8080"
	}
	if cfg.Server.MaxBodyBytes == 0 {
		cfg.Server.MaxBodyBytes = 10 << 20 // 10 MB default
	}
	if cfg.Interceptor.SoftLimit == 0 {
		cfg.Interceptor.SoftLimit = 4000 // characters
	}
	if cfg.Interceptor.HardLimit == 0 {
		cfg.Interceptor.HardLimit = 24000 // characters
	}
	if cfg.Interceptor.TruncationStrategy == "" {
		cfg.Interceptor.TruncationStrategy = "middle-out"
	}
	if cfg.Interceptor.KeepFirstPercent == 0 {
		cfg.Interceptor.KeepFirstPercent = 15.0
	}
	if cfg.Interceptor.KeepLastPercent == 0 {
		cfg.Interceptor.KeepLastPercent = 25.0
	}
	if cfg.Ollama.URL == "" {
		cfg.Ollama.URL = "http://127.0.0.1:11434/api/generate"
	}
	if cfg.Ollama.Model == "" {
		cfg.Ollama.Model = "qwen2.5-coder:7b"
	}
	if cfg.Ollama.SystemPrompt == "" {
		cfg.Ollama.SystemPrompt = "You are a data privacy filter. Summarize the following log/code error in 5 lines. REMOVE any IP addresses, AWS keys (AKIA...), or passwords. Keep only the technical core of the error."
	}

	return &cfg, nil
}

// loadSecrets reads the JSON secrets file from systemd credentials.
func loadSecrets() (*SecretsConfig, error) {
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	if credDir == "" {
		return nil, fmt.Errorf("CREDENTIALS_DIRECTORY not set")
	}
	secretsPath := filepath.Join(credDir, "secrets")
	data, err := os.ReadFile(secretsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read secrets file %s: %v", secretsPath, err)
	}

	var secrets SecretsConfig
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("failed to parse secrets JSON: %v", err)
	}

	// Validate required fields
	if secrets.ClientToken == "" {
		return nil, fmt.Errorf("client_token missing in secrets")
	}
	// API keys can be empty if not using that upstream
	// but warn if they might be needed

	return &secrets, nil
}

// rateLimiter tracks token and request counts per minute.
type rateLimiter struct {
	tpmTokens int       // token bucket for TPM
	rpmCount  int       // counter for RPM
	lastReset time.Time
	mu        sync.RWMutex
}

// NenyaGateway encapsulates the proxy state and HTTP client.
type NenyaGateway struct {
	config     Config
	client     *http.Client
	tokenizer  *tiktoken.Tiktoken
	secrets    *SecretsConfig
	rateLimits map[string]*rateLimiter // keyed by upstream host
	rlMu       sync.RWMutex
}

// NewNenyaGateway initializes the proxy with strict security settings.
func NewNenyaGateway(cfg Config, secrets *SecretsConfig) *NenyaGateway {
	// Security: Custom HTTP client with strict timeouts to prevent connection hanging.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}

	secureClient := &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second, // Total request timeout
	}

	var tokenizer *tiktoken.Tiktoken
	tokenizer, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		log.Printf("[WARN] Failed to initialize cl100k_base tokenizer: %v. Falling back to byte length counting.", err)
		tokenizer = nil
	}

	return &NenyaGateway{
		config:     cfg,
		client:     secureClient,
		tokenizer:  tokenizer,
		secrets:    secrets,
		rateLimits: make(map[string]*rateLimiter),
	}
}

// countTokens returns the number of tokens in text using cl100k_base if available,
// otherwise falls back to byte-length approximation (bytes / 4).
func (g *NenyaGateway) countTokens(text string) int {
	if g.tokenizer != nil {
		tokens := g.tokenizer.Encode(text, nil, nil)
		return len(tokens)
	}
	// Fallback: rough approximation for English text (4 chars ≈ 1 token)
	// This is inaccurate for non-English text but better than nothing
	return len(text) / 4
}

// truncateMiddleOut reduces text to maxSize runes, keeping first KeepFirstPercent% and last KeepLastPercent%.
// Implements UTF-8 safe middle‑out truncation for massive payloads.
func (g *NenyaGateway) truncateMiddleOut(text string, maxSize int) string {
	runes := []rune(text)
	if len(runes) <= maxSize {
		return text
	}

	separator := "\n... [NENYA: MASSIVE PAYLOAD TRUNCATED] ...\n"
	separatorRunes := []rune(separator)
	separatorLen := len(separatorRunes)

	// Available space for content after reserving space for separator
	available := maxSize - separatorLen
	if available <= 0 {
		// Not enough space for separator; just return separator truncated to maxSize
		return string(separatorRunes[:maxSize])
	}

	keepFirst := int(float64(maxSize) * g.config.Interceptor.KeepFirstPercent / 100.0)
	keepLast := int(float64(maxSize) * g.config.Interceptor.KeepLastPercent / 100.0)

	// Adjust if keepFirst + keepLast exceeds available space
	if keepFirst + keepLast > available {
		// Scale down proportionally
		total := keepFirst + keepLast
		keepFirst = keepFirst * available / total
		keepLast = available - keepFirst
	}

	// Ensure at least some content from both ends
	if keepFirst == 0 && keepLast > 0 {
		keepFirst = 1
		keepLast = available - 1
	} else if keepLast == 0 && keepFirst > 0 {
		keepLast = 1
		keepFirst = available - 1
	} else if keepFirst == 0 && keepLast == 0 {
		// No space for content, just return separator (already handled by available <=0)
		keepFirst = 0
		keepLast = 0
	}

	result := make([]rune, 0, maxSize)
	result = append(result, runes[:keepFirst]...)
	result = append(result, separatorRunes...)
	result = append(result, runes[len(runes)-keepLast:]...)

	// Final length should be exactly keepFirst + separatorLen + keepLast == maxSize
	return string(result)
}

// checkRateLimit verifies if the request is within RPM/TPM limits for the given upstream.
// Returns true if allowed, false if rate limited.
func (g *NenyaGateway) checkRateLimit(upstreamURL string, tokenCount int) bool {
	// Extract host for rate limit key (simplified)
	host := upstreamURL
	if idx := strings.Index(upstreamURL, "://"); idx != -1 {
		host = upstreamURL[idx+3:]
	}
	if idx := strings.Index(host, "/"); idx != -1 {
		host = host[:idx]
	}

	g.rlMu.Lock()
	defer g.rlMu.Unlock()

	limiter, exists := g.rateLimits[host]
	if !exists {
		limiter = &rateLimiter{
			lastReset: time.Now(),
		}
		g.rateLimits[host] = limiter
	}

	// Reset counters if more than a minute has passed
	if time.Since(limiter.lastReset) > time.Minute {
		limiter.tpmTokens = 0
		limiter.rpmCount = 0
		limiter.lastReset = time.Now()
	}

	// Check RPM limit
	if g.config.RateLimit.MaxRPM > 0 && limiter.rpmCount >= g.config.RateLimit.MaxRPM {
		log.Printf("[RATELIMIT] RPM limit exceeded for %s: %d >= %d", host, limiter.rpmCount, g.config.RateLimit.MaxRPM)
		return false
	}

	// Check TPM limit (token count is approximate)
	if g.config.RateLimit.MaxTPM > 0 && limiter.tpmTokens+tokenCount > g.config.RateLimit.MaxTPM {
		log.Printf("[RATELIMIT] TPM limit exceeded for %s: %d + %d > %d", host, limiter.tpmTokens, tokenCount, g.config.RateLimit.MaxTPM)
		return false
	}

	// Update counters
	limiter.rpmCount++
	limiter.tpmTokens += tokenCount
	return true
}

// ServeHTTP makes NenyaGateway satisfy the http.Handler interface.
func (g *NenyaGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	g.handleChatCompletions(w, r)
}

// determineUpstream dynamically routes the request based on the model name.
func (g *NenyaGateway) determineUpstream(modelName string) string {
	modelName = strings.ToLower(modelName)

	if strings.HasPrefix(modelName, "gemini-") {
		// Gemini OpenAI‑compatible endpoint (v1, not v1beta)
		return "https://generativelanguage.googleapis.com/v1/openai/chat/completions"
	}
	if strings.HasPrefix(modelName, "deepseek-") {
		// e.g., deepseek-reasoner, deepseek-chat
		return "https://api.deepseek.com/v1/chat/completions"
	}
	// Fallback/Default upstream (z.ai for glm-5, etc.)
	return "https://api.z.ai/v1/chat/completions"
}

func (g *NenyaGateway) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Security: Prevent DoS by limiting the request body size.
	r.Body = http.MaxBytesReader(w, r.Body, g.config.Server.MaxBodyBytes)

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read request body: %v", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}
	defer r.Body.Close()

	// Client Authentication
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		log.Printf("[WARN] Missing or malformed Authorization header")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	clientToken := strings.TrimPrefix(authHeader, "Bearer ")
	if clientToken != g.secrets.ClientToken {
		log.Printf("[WARN] Invalid client token")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Parse the JSON request to extract the model and messages
	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		log.Printf("[WARN] Failed to parse JSON, returning Bad Request.")
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// 1. Dynamic Routing: Identify the requested model
	modelName, ok := payload["model"].(string)
	if !ok {
		modelName = "glm-5" // Default safe fallback
	}
	upstreamURL := g.determineUpstream(modelName)
	log.Printf("[INFO] Model requested: '%s' -> Routing to: %s", modelName, upstreamURL)

	// 2. The Bouncer (Payload Interception) – 3‑Tier Pipeline
	if messagesRaw, ok := payload["messages"]; ok {
		if messages, ok := messagesRaw.([]interface{}); ok && len(messages) > 0 {
			lastMsgRaw := messages[len(messages)-1]
			if lastMsgNode, ok := lastMsgRaw.(map[string]interface{}); ok {
				if contentRaw, ok := lastMsgNode["content"]; ok {
					if content, ok := contentRaw.(string); ok {
						contentRunes := utf8.RuneCountInString(content)
						softLimit := g.config.Interceptor.SoftLimit
						hardLimit := g.config.Interceptor.HardLimit

						// Tier 1: Pass‑through (no intervention)
						if contentRunes < softLimit {
							log.Printf("[INFO] Payload within soft limit (%d runes < %d), passing through", contentRunes, softLimit)
						} else if contentRunes <= hardLimit {
							// Tier 2: Ollama only (The Scalpel)
							log.Printf("[WARN] Payload exceeds soft limit (%d runes), sending to Ollama for summarization...", contentRunes)
							summarizedContent, err := g.summarizeWithOllama(content)
							if err != nil {
								log.Printf("[ERROR] Ollama summarization failed: %v. Proceeding with original payload.", err)
							} else {
								// Mutate the payload with the sanitized content
								lastMsgNode["content"] = fmt.Sprintf("[Nenya Sanitized via Ollama]:\n%s", summarizedContent)
								newBodyBytes, err := json.Marshal(payload)
								if err != nil {
									log.Printf("[ERROR] Failed to marshal updated payload: %v. Using original payload.", err)
								} else {
									bodyBytes = newBodyBytes
								}
							}
						} else {
							// Tier 3: Truncate + Ollama (The Chainsaw)
							log.Printf("[WARN] Payload exceeds hard limit (%d runes > %d), applying middle‑out truncation before Ollama...", contentRunes, hardLimit)
							truncated := g.truncateMiddleOut(content, hardLimit)
							summarizedContent, err := g.summarizeWithOllama(truncated)
							if err != nil {
								log.Printf("[ERROR] Ollama summarization failed after truncation: %v. Forwarding truncated payload.", err)
								// Fallback: send truncated content without summarization
								lastMsgNode["content"] = fmt.Sprintf("[Nenya Truncated (Ollama unreachable)]:\n%s", truncated)
								newBodyBytes, err := json.Marshal(payload)
								if err != nil {
									log.Printf("[ERROR] Failed to marshal truncated payload: %v. Using original payload.", err)
								} else {
									bodyBytes = newBodyBytes
								}
							} else {
								// Mutate with summarized version
								lastMsgNode["content"] = fmt.Sprintf("[Nenya Sanitized via Ollama (truncated input)]:\n%s", summarizedContent)
								newBodyBytes, err := json.Marshal(payload)
								if err != nil {
									log.Printf("[ERROR] Failed to marshal updated payload: %v. Using original payload.", err)
								} else {
									bodyBytes = newBodyBytes
								}
							}
						}
					} else {
						log.Printf("[WARN] Last message content is not a string, skipping Ollama interception")
					}
				}
			} else {
				log.Printf("[WARN] Last message is not a map, skipping Ollama interception")
			}
		} else {
			log.Printf("[WARN] Messages field is not a non-empty array, skipping Ollama interception")
		}
	}

	// 3. Rate Limiting
	tokenCount := g.countTokens(string(bodyBytes))
	if !g.checkRateLimit(upstreamURL, tokenCount) {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// 4. Forward to the chosen Upstream
	g.forwardToUpstream(w, r, upstreamURL, bodyBytes)
}

// summarizeWithOllama handles the local AI inference request to redact/summarize.
func (g *NenyaGateway) summarizeWithOllama(heavyText string) (string, error) {
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

	req, err := http.NewRequest(http.MethodPost, g.config.Ollama.URL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create ollama request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
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

// transformRequestForUpstream modifies the request body for specific upstream providers.
func (g *NenyaGateway) transformRequestForUpstream(upstreamURL string, body []byte) ([]byte, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse request body for transformation: %v", err)
	}

	modelRaw, ok := payload["model"]
	if !ok {
		return body, nil // No model field, nothing to transform
	}

	modelName, ok := modelRaw.(string)
	if !ok {
		return body, nil // Model is not a string
	}

	// Gemini OpenAI‑compatible endpoint model mapping
	if strings.Contains(upstreamURL, "generativelanguage.googleapis.com") {
		// Map Gemini model names to what the OpenAI‑compatible endpoint expects
		geminiModelMap := map[string]string{
			"gemini-3-flash":          "gemini-1.5-flash",    // Map to supported model
			"gemini-3.1-flash-lite":   "gemini-1.5-flash",
			"gemini-3-flash-thinking": "gemini-1.5-flash",
			"gemini-pro":              "gemini-1.5-pro",
			"gemini-1.5-pro":          "gemini-1.5-pro",
			"gemini-1.5-flash":        "gemini-1.5-flash",
		}

		lowerModel := strings.ToLower(modelName)
		if mapped, ok := geminiModelMap[lowerModel]; ok {
			payload["model"] = mapped
			log.Printf("[INFO] Mapping Gemini model: %s -> %s", modelName, mapped)
		} else if strings.HasPrefix(lowerModel, "gemini-") {
			// Default fallback: try removing version prefix
			transformed := strings.TrimPrefix(lowerModel, "gemini-")
			transformed = strings.TrimPrefix(transformed, "1.5-")
			transformed = strings.TrimPrefix(transformed, "2.0-")
			payload["model"] = transformed
			log.Printf("[INFO] Transforming Gemini model name: %s -> %s", modelName, transformed)
		}
	}

	newBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal transformed request: %v", err)
	}

	return newBody, nil
}

// forwardToUpstream handles the reverse proxy logic and SSE streaming.
func (g *NenyaGateway) forwardToUpstream(w http.ResponseWriter, r *http.Request, upstreamURL string, body []byte) {
	// Transform request body if needed for upstream compatibility
	transformedBody, err := g.transformRequestForUpstream(upstreamURL, body)
	if err != nil {
		log.Printf("[WARN] Failed to transform request for upstream: %v. Using original body.", err)
		transformedBody = body
	}

	req, err := http.NewRequest(r.Method, upstreamURL, bytes.NewBuffer(transformedBody))
	if err != nil {
		log.Printf("[ERROR] Failed to create upstream request: %v", err)
		http.Error(w, "Internal Gateway Error", http.StatusInternalServerError)
		return
	}

	// Security: Remove client authentication header before forwarding
	headers := r.Header.Clone()
	headers.Del("Authorization")

	// Inject appropriate upstream API key
	switch {
	case strings.Contains(upstreamURL, "generativelanguage.googleapis.com"):
		if g.secrets.GeminiKey != "" {
			// Gemini expects x-goog-api-key header for API keys (not Authorization)
			headers.Set("x-goog-api-key", g.secrets.GeminiKey)
			// Also set Authorization for compatibility
			headers.Set("Authorization", "Bearer "+g.secrets.GeminiKey)
		} else {
			log.Printf("[ERROR] Gemini API key missing in secrets")
			http.Error(w, "Gateway configuration error", http.StatusInternalServerError)
			return
		}
	case strings.Contains(upstreamURL, "api.deepseek.com"):
		if g.secrets.DeepSeekKey != "" {
			headers.Set("Authorization", "Bearer "+g.secrets.DeepSeekKey)
		} else {
			log.Printf("[ERROR] DeepSeek API key missing in secrets")
			http.Error(w, "Gateway configuration error", http.StatusInternalServerError)
			return
		}
	case strings.Contains(upstreamURL, "api.z.ai"):
		if g.secrets.ZaiKey != "" {
			headers.Set("Authorization", "Bearer "+g.secrets.ZaiKey)
		} else {
			log.Printf("[ERROR] z.ai API key missing in secrets")
			http.Error(w, "Gateway configuration error", http.StatusInternalServerError)
			return
		}
	default:
		log.Printf("[ERROR] Unknown upstream URL: %s", upstreamURL)
		http.Error(w, "Gateway routing error", http.StatusInternalServerError)
		return
	}

	// Copy sanitized headers
	copyHeaders(headers, req.Header)

	resp, err := g.client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Upstream request failed (%s): %v", upstreamURL, err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	// Stream the response back to the client natively
	io.Copy(w, resp.Body)
}

// Helper function to safely copy headers.
func copyHeaders(src, dst http.Header) {
	hopByHopHeaders := map[string]bool{
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

	for k, vv := range src {
		lowerKey := strings.ToLower(k)
		if hopByHopHeaders[lowerKey] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", "config.toml", "Path to configuration file")
	flag.Parse()

	cfg, err := loadConfig(configFile)
	if err != nil {
		log.Fatalf("[FATAL] Failed to load configuration: %v", err)
	}

	secrets, err := loadSecrets()
	if err != nil {
		log.Fatalf("[FATAL] Failed to load secrets: %v", err)
	}

	gateway := NewNenyaGateway(*cfg, secrets)

	// Security: Strict server timeouts
	srv := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      gateway,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("💍 Nenya AI Gateway is listening on %s", cfg.Server.ListenAddr)
	log.Printf("   -> Supported routes: DeepSeek (deepseek-*), Gemini (gemini-*), z.ai (glm-*)")

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("[FATAL] Server failed: %v", err)
	}
}
