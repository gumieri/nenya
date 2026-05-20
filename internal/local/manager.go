package local

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"git.0ur.uk/nenya/config"
)

type EngineManager struct {
	sessions *SessionManager
	config   *config.LocalEngineConfig
	mu       sync.RWMutex
	logger   *slog.Logger
}

func NewEngineManager(cfg *config.LocalEngineConfig, logger *slog.Logger) *EngineManager {
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 3
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	return &EngineManager{
		sessions: NewSessionManager(cfg.BaseURL, timeout),
		config:   cfg,
		logger:   logger,
	}
}

func (em *EngineManager) Startup(ctx context.Context) error {
	if len(em.config.StartupModels) == 0 {
		return nil
	}

	em.logger.Info("loading startup models", "models", em.config.StartupModels)

	for _, modelID := range em.config.StartupModels {
		if err := em.LoadModel(ctx, modelID, LoadOptions{}); err != nil {
			em.logger.Warn("failed to load startup model", "model", modelID, "error", err)
		} else {
			em.logger.Info("startup model loaded", "model", modelID)
		}
	}

	return nil
}

func (em *EngineManager) LoadModel(ctx context.Context, modelID string, opts LoadOptions) error {
	em.mu.Lock()
	defer em.mu.Unlock()

	if em.sessions.IsLoaded(modelID) {
		return nil
	}

	if len(em.sessions.GetLoadedModels()) >= em.config.MaxSessions {
		if !evictLRU(em.sessions, em.logger) {
			return fmt.Errorf("max sessions (%d) reached and no session to evict", em.config.MaxSessions)
		}
	}

	session, err := em.sessions.LoadModel(ctx, modelID, opts)
	if err != nil {
		return fmt.Errorf("load model %q: %w", modelID, err)
	}

	em.logger.Info("model loaded", "model", modelID, "is_embedding", session.IsEmbedding)
	return nil
}

func (em *EngineManager) UnloadModel(ctx context.Context, modelID string) error {
	em.mu.Lock()
	defer em.mu.Unlock()

	if !em.sessions.IsLoaded(modelID) {
		return nil
	}

	if err := em.sessions.UnloadModel(ctx, modelID); err != nil {
		return fmt.Errorf("unload model %q: %w", modelID, err)
	}

	em.logger.Info("model unloaded", "model", modelID)
	return nil
}

func (em *EngineManager) IsLoaded(modelID string) bool {
	return em.sessions.IsLoaded(modelID)
}

func (em *EngineManager) ListInstalledModels(ctx context.Context) ([]ModelInfo, error) {
	return em.sessions.ListInstalledModels(ctx)
}

func (em *EngineManager) GetLoadedModels() []string {
	return em.sessions.GetLoadedModels()
}

func evictLRU(sm *SessionManager, logger *slog.Logger) bool {
	models := sm.GetLoadedModels()
	if len(models) == 0 {
		return false
	}

	oldestModel := ""
	var oldestTime time.Time

	sm.mu.RLock()
	for modelID, session := range sm.sessions {
		if oldestModel == "" || session.CreatedAt.Before(oldestTime) {
			oldestModel = modelID
			oldestTime = session.CreatedAt
		}
	}
	sm.mu.RUnlock()

	if oldestModel == "" {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sm.UnloadModel(ctx, oldestModel); err != nil {
		logger.Warn("eviction failed for model", "model", oldestModel, "error", err)
		return false
	}

	logger.Info("evicted LRU model", "model", oldestModel)
	return true
}
