package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nenya/config"
)

// JSONFileStorage provides JSON file-based persistence for provider accounts.
// Each provider's accounts are stored in a separate file: <provider>.accounts.json
//
// WARNING: Credential fields are persisted in plaintext. This storage backend
// is intended for operational state (backoff levels, cooldowns, last_used timestamps)
// when credentials are supplied from secrets.json at startup. Do NOT enable this
// backend if credentials must be encrypted at rest. See §4 of AGENTS.md.
type JSONFileStorage struct {
	configDir string
	mu        sync.Mutex
}

// NewJSONFileStorage creates a new JSON file storage backend.
func NewJSONFileStorage(configDir string) *JSONFileStorage {
	return &JSONFileStorage{configDir: configDir}
}

// LoadAccounts loads all accounts for a provider from JSON file.
// Returns nil if the file doesn't exist (not an error).
func (s *JSONFileStorage) LoadAccounts(provider string) ([]*config.ProviderAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.accountFilePath(provider)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var accounts []*config.ProviderAccount
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, err
	}

	for _, acc := range accounts {
		if acc.ModelLocks == nil {
			acc.ModelLocks = make(map[string]time.Time)
		}
		if acc.CreatedAt.IsZero() {
			acc.CreatedAt = time.Now()
		}
	}

	return accounts, nil
}

// SaveAccount persists an account's state to the JSON file.
// Updates the account if it exists, appends if it doesn't.
func (s *JSONFileStorage) SaveAccount(provider string, account *config.ProviderAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.accountFilePath(provider)
	if err != nil {
		return err
	}

	var accounts []*config.ProviderAccount
	if data, readErr := os.ReadFile(path); readErr == nil {
		if unmarshalErr := json.Unmarshal(data, &accounts); unmarshalErr != nil {
			return unmarshalErr
		}
	}

	found := false
	for i, acc := range accounts {
		if acc.ID == account.ID {
			accounts[i] = account
			found = true
			break
		}
	}

	if !found {
		accounts = append(accounts, account)
	}

	data, err := json.MarshalIndent(accounts, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

// accountFilePath returns the full path to the account JSON file for a provider.
// Validates that the resolved path does not escape the config directory.
func (s *JSONFileStorage) accountFilePath(provider string) (string, error) {
	filename := provider + ".accounts.json"
	full := filepath.Join(s.configDir, filename)

	abs, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("failed to resolve absolute path: %w", err)
	}
	configAbs, err := filepath.Abs(s.configDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve config dir: %w", err)
	}
	if !strings.HasPrefix(abs, configAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("provider name %q escapes config directory", provider)
	}

	return abs, nil
}

// DeleteProvider removes all accounts for a provider by deleting the JSON file.
func (s *JSONFileStorage) DeleteProvider(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.accountFilePath(provider)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ListProviders returns all provider names that have account files.
func (s *JSONFileStorage) ListProviders() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.configDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}

	var providers []string
	const suffix = ".accounts.json"
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			providers = append(providers, name[:len(name)-len(suffix)])
		}
	}

	return providers, nil
}
