package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTopK      = 10
	defaultThreshold = 0.3
	searchTimeout    = 5 * time.Second
	addTimeout       = 10 * time.Second
)

type MemoryConfig struct {
	URL       string  `json:"url"`
	APIKey    string  `json:"api_key,omitempty"`
	UserID    string  `json:"user_id"`
	TopK      int     `json:"top_k"`
	Threshold float64 `json:"threshold"`
}

func (c *MemoryConfig) effectiveTopK() int {
	if c.TopK > 0 {
		return c.TopK
	}
	return defaultTopK
}

func (c *MemoryConfig) effectiveThreshold() float64 {
	if c.Threshold > 0 {
		return c.Threshold
	}
	return defaultThreshold
}

func (c *MemoryConfig) baseURL() string {
	return strings.TrimRight(c.URL, "/")
}

type MemoryResult struct {
	ID     string  `json:"id"`
	Memory string  `json:"memory"`
	Score  float64 `json:"score,omitempty"`
}

type searchRequest struct {
	Query     string  `json:"query"`
	UserID    string  `json:"user_id,omitempty"`
	TopK      int     `json:"top_k,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}

type searchResponse struct {
	Results []MemoryResult `json:"results"`
}

type addRequest struct {
	Messages []AddMessage `json:"messages"`
	UserID   string       `json:"user_id,omitempty"`
}

type AddMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type addResponse struct {
	Results []addResult `json:"results"`
}

type addResult struct {
	ID     string `json:"id"`
	Memory string `json:"memory"`
	Event  string `json:"event"`
}

type Mem0Client struct {
	httpClient *http.Client
	config     MemoryConfig
	logger     *slog.Logger
}

func NewMem0Client(cfg MemoryConfig, logger *slog.Logger) *Mem0Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          5,
		MaxIdleConnsPerHost:   2,
	}
	return &Mem0Client{
		httpClient: &http.Client{Transport: transport},
		config:     cfg,
		logger:     logger,
	}
}

func (c *Mem0Client) Search(ctx context.Context, query string) ([]MemoryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	reqBody := searchRequest{
		Query:     query,
		UserID:    c.config.UserID,
		TopK:      c.config.effectiveTopK(),
		Threshold: c.config.effectiveThreshold(),
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal search request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.baseURL()+"/search", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("search returned %d: %s", resp.StatusCode, string(respBody))
	}

	var searchResp searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	return searchResp.Results, nil
}

func (c *Mem0Client) Add(ctx context.Context, messages []AddMessage) error {
	if len(messages) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, addTimeout)
	defer cancel()

	reqBody := addRequest{
		Messages: messages,
		UserID:   c.config.UserID,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal add request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.baseURL()+"/memories", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create add request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("add request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("add returned %d: %s", resp.StatusCode, string(respBody))
	}

	var addResp addResponse
	if err := json.NewDecoder(resp.Body).Decode(&addResp); err != nil {
		return fmt.Errorf("decode add response: %w", err)
	}

	if c.logger != nil && len(addResp.Results) > 0 {
		for _, r := range addResp.Results {
			if r.Event != "NONE" {
				c.logger.Debug("memory stored",
					"id", r.ID, "event", r.Event, "memory", truncateLog(r.Memory, 120))
			}
		}
	}

	return nil
}

func (c *Mem0Client) setAuth(req *http.Request) {
	if c.config.APIKey != "" {
		req.Header.Set("X-API-Key", c.config.APIKey)
	}
}

func truncateLog(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func FormatMemoryContext(results []MemoryResult) string {
	if len(results) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[Relevant memory context]\n")
	for _, r := range results {
		if r.Memory == "" {
			continue
		}
		fmt.Fprintf(&sb, "- %s\n", r.Memory)
	}
	return sb.String()
}

type ContentBuilder struct {
	buf strings.Builder
}

func NewContentBuilder() *ContentBuilder {
	return &ContentBuilder{}
}

func (b *ContentBuilder) AddContent(content string) {
	b.buf.WriteString(content)
}

func (b *ContentBuilder) Build() string {
	return b.buf.String()
}
