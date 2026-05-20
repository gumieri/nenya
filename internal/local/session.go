// Package local manages the lifecycle of local Ollama models for the Nenya
// gateway. It provides model loading/unloading into GPU memory, session
// tracking with LRU eviction, startup preloading, and integration with the
// routing layer via the LocalEngineCheck interface.
//
// # Lock Ordering
//
// The EngineManager uses two mutexes with a strict ordering to prevent
// deadlocks: em.mu (EngineManager) must be acquired BEFORE sm.mu
// (SessionManager). This ordering is maintained throughout all methods.
//
// # Architecture
//
// Local models are NOT routed through a separate code path. They flow
// through the existing retryLoop → prepareAndSend → streamResponse pipeline
// as standard UpstreamTargets. The SessionManager only handles load/unload
// lifecycle; chat completions are proxied by the existing Ollama provider
// adapter and OllamaTransformer.
package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"git.0ur.uk/nenya/internal/util"
)

const maxRetryAttempts = 3
const maxResponseBytes = 64 << 20

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	client   *http.Client
	baseURL  string
	timeout  time.Duration
}

type Session struct {
	ModelID     string
	Port        int
	PID         int
	IsEmbedding bool
	MMProjPath  string
	CreatedAt   time.Time
}

type ModelInfo struct {
	ID       string
	Size     int64
	Modified time.Time
}

type LoadOptions struct {
	MMProjPath       string
	NumGPU           int
	NumCtx           int
	IsEmbedding      bool
	BypassAutoUnload bool
}

type generateRequest struct {
	Model     string         `json:"model"`
	Stream    bool           `json:"stream"`
	KeepAlive int            `json:"keep_alive"`
	Options   map[string]any `json:"options,omitempty"`
}

type generateResponse struct {
	Done     bool   `json:"done"`
	Response string `json:"response"`
}

type tagsResponse struct {
	Models []struct {
		Name       string `json:"name"`
		Size       int64  `json:"size"`
		ModifiedAt string `json:"modified_at"`
	} `json:"models"`
}

func NewSessionManager(baseURL string, timeout time.Duration) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConns:          10,
				MaxIdleConnsPerHost:   2,
			},
		},
		baseURL: baseURL,
		timeout: timeout,
	}
}

func (sm *SessionManager) LoadModel(ctx context.Context, modelID string, opts LoadOptions) (*Session, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if existing, ok := sm.sessions[modelID]; ok {
		return existing, nil
	}

	payload := generateRequest{
		Model:     modelID,
		Stream:    false,
		KeepAlive: -1,
	}

	if opts.NumGPU > 0 {
		if payload.Options == nil {
			payload.Options = make(map[string]any)
		}
		payload.Options["num_gpu"] = opts.NumGPU
	}

	if opts.NumCtx > 0 {
		if payload.Options == nil {
			payload.Options = make(map[string]any)
		}
		payload.Options["num_ctx"] = opts.NumCtx
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal load request: %w", err)
	}

	var responseBody []byte
	err = util.DoWithRetry(ctx, maxRetryAttempts, func() error {
		var loadErr error
		req, loadErr := http.NewRequestWithContext(ctx, http.MethodPost, sm.baseURL+"/api/generate", bytes.NewReader(body))
		if loadErr != nil {
			return loadErr
		}
		req.Header.Set("Content-Type", "application/json")

		resp, loadErr := sm.client.Do(req)
		if loadErr != nil {
			return loadErr
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("ollama generate failed: %d", resp.StatusCode)
		}

		responseBody, loadErr = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if loadErr != nil {
			return loadErr
		}

		var gr generateResponse
		if loadErr := json.Unmarshal(responseBody, &gr); loadErr != nil {
			return loadErr
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("load model: %w", err)
	}

	session := &Session{
		ModelID:     modelID,
		IsEmbedding: opts.IsEmbedding,
		MMProjPath:  opts.MMProjPath,
		CreatedAt:   time.Now(),
	}

	sm.sessions[modelID] = session
	return session, nil
}

func (sm *SessionManager) UnloadModel(ctx context.Context, modelID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, ok := sm.sessions[modelID]; !ok {
		return nil
	}

	payload := generateRequest{
		Model:     modelID,
		Stream:    false,
		KeepAlive: 0,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal unload request: %w", err)
	}

	err = util.DoWithRetry(ctx, maxRetryAttempts, func() error {
		var loadErr error
		req, loadErr := http.NewRequestWithContext(ctx, http.MethodPost, sm.baseURL+"/api/generate", bytes.NewReader(body))
		if loadErr != nil {
			return loadErr
		}
		req.Header.Set("Content-Type", "application/json")

		resp, loadErr := sm.client.Do(req)
		if loadErr != nil {
			return loadErr
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("ollama generate failed: %d", resp.StatusCode)
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("unload model: %w", err)
	}

	delete(sm.sessions, modelID)
	return nil
}

func (sm *SessionManager) IsLoaded(modelID string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	_, ok := sm.sessions[modelID]
	return ok
}

func (sm *SessionManager) GetLoadedModels() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	models := make([]string, 0, len(sm.sessions))
	for modelID := range sm.sessions {
		models = append(models, modelID)
	}
	return models
}

func (sm *SessionManager) ListInstalledModels(ctx context.Context) ([]ModelInfo, error) {
	var responseBody []byte
	err := util.DoWithRetry(ctx, maxRetryAttempts, func() error {
		var loadErr error
		req, loadErr := http.NewRequestWithContext(ctx, http.MethodGet, sm.baseURL+"/api/tags", nil)
		if loadErr != nil {
			return loadErr
		}

		resp, loadErr := sm.client.Do(req)
		if loadErr != nil {
			return loadErr
		}
		defer func() {
			_ = resp.Body.Close()
		}()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("ollama tags failed: %d", resp.StatusCode)
		}

		responseBody, loadErr = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if loadErr != nil {
			return loadErr
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("list installed models: %w", err)
	}

	var tr tagsResponse
	if err := json.Unmarshal(responseBody, &tr); err != nil {
		return nil, fmt.Errorf("unmarshal tags response: %w", err)
	}

	models := make([]ModelInfo, 0, len(tr.Models))
	for _, m := range tr.Models {
		modified, err := time.Parse(time.RFC3339, m.ModifiedAt)
		if err != nil {
			modified = time.Time{}
		}
		models = append(models, ModelInfo{
			ID:       m.Name,
			Size:     m.Size,
			Modified: modified,
		})
	}

	return models, nil
}
