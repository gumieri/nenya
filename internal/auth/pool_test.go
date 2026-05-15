package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"nenya/config"
)

func newTestAccount(id, credential string) *config.ProviderAccount {
	return &config.ProviderAccount{
		ID:             id,
		CredentialType: config.CredentialTypeAPIKey,
		Credential:     credential,
		Status:         config.AccountStatusActive,
		ModelLocks:     make(map[string]time.Time),
		CreatedAt:      time.Now(),
	}
}

func TestAccountPool_SelectAccount_LRU(t *testing.T) {
	accounts := []*config.ProviderAccount{
		newTestAccount("a1", "key1"),
		newTestAccount("a2", "key2"),
		newTestAccount("a3", "key3"),
	}

	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()

	acc1, err := pool.SelectAccount(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc1.ID != "a1" {
		t.Errorf("expected a1, got %s", acc1.ID)
	}

	acc2, err := pool.SelectAccount(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc2.ID != "a2" {
		t.Errorf("expected a2, got %s", acc2.ID)
	}

	acc3, err := pool.SelectAccount(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc3.ID != "a3" {
		t.Errorf("expected a3, got %s", acc3.ID)
	}

	acc4, err := pool.SelectAccount(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc4.ID != "a1" {
		t.Errorf("expected a1 (LRU cycle), got %s", acc4.ID)
	}
}

func TestAccountPool_SelectAccount_SkipsRateLimited(t *testing.T) {
	accounts := []*config.ProviderAccount{
		newTestAccount("a1", "key1"),
		{ID: "a2", CredentialType: config.CredentialTypeAPIKey, Credential: "key2", Status: config.AccountStatusActive, ModelLocks: make(map[string]time.Time), RateLimitedUntil: time.Now().Add(1 * time.Hour), CreatedAt: time.Now()},
	}

	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()

	acc, err := pool.SelectAccount(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc.ID != "a1" {
		t.Errorf("expected a1, got %s", acc.ID)
	}
}

func TestAccountPool_SelectAccount_SkipsDisabled(t *testing.T) {
	accounts := []*config.ProviderAccount{
		{ID: "a1", CredentialType: config.CredentialTypeAPIKey, Credential: "key1", Status: config.AccountStatusDisabled, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
		newTestAccount("a2", "key2"),
	}

	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()

	acc, err := pool.SelectAccount(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc.ID != "a2" {
		t.Errorf("expected a2 (a1 is disabled), got %s", acc.ID)
	}
}

func TestAccountPool_SelectAccount_SkipsModelLocked(t *testing.T) {
	modelLocks := map[string]time.Time{
		"gpt-4": time.Now().Add(1 * time.Hour),
	}
	accounts := []*config.ProviderAccount{
		{ID: "a1", CredentialType: config.CredentialTypeAPIKey, Credential: "key1", Status: config.AccountStatusActive, ModelLocks: modelLocks, CreatedAt: time.Now()},
		newTestAccount("a2", "key2"),
	}

	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()

	acc, err := pool.SelectAccount(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc.ID != "a2" {
		t.Errorf("expected a2 (a1 is model locked), got %s", acc.ID)
	}

	acc, err = pool.SelectAccount(ctx, "claude-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc.ID != "a1" {
		t.Errorf("expected a1 (not locked for claude-3), got %s", acc.ID)
	}
}

func TestAccountPool_SelectAccount_NoAvailableAccounts(t *testing.T) {
	accounts := []*config.ProviderAccount{
		{ID: "a1", CredentialType: config.CredentialTypeAPIKey, Credential: "key1", Status: config.AccountStatusError, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
	}

	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()

	_, err := pool.SelectAccount(ctx, "gpt-4")
	if err == nil {
		t.Error("expected error when no accounts available")
	}

	noAccountErr, ok := err.(*NoAvailableAccountError)
	if !ok {
		t.Errorf("expected NoAvailableAccountError, got %T", err)
	}
	if noAccountErr.Provider != "test-provider" {
		t.Errorf("expected provider 'test-provider', got %s", noAccountErr.Provider)
	}
}

func TestAccountPool_SelectAccount_ReturnsCredential(t *testing.T) {
	accounts := []*config.ProviderAccount{
		newTestAccount("a1", "secret-key-123"),
	}

	pool := NewAccountPool("test-provider", accounts)
	acc, err := pool.SelectAccount(context.Background(), "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc.Credential != "secret-key-123" {
		t.Errorf("expected credential 'secret-key-123', got %s", acc.Credential)
	}
}

func TestAccountPool_ApplyError_429(t *testing.T) {
	accounts := []*config.ProviderAccount{
		newTestAccount("a1", "key1"),
	}

	pool := NewAccountPool("test-provider", accounts)

	err := pool.ApplyError("a1", 429, "rate limited")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	acc := pool.GetAccount("a1")
	if acc.Status != config.AccountStatusError {
		t.Errorf("expected error status, got %s", acc.Status)
	}
	if acc.BackoffLevel != 1 {
		t.Errorf("expected backoff level 1, got %d", acc.BackoffLevel)
	}
	if acc.RateLimitedUntil.IsZero() {
		t.Error("expected rate limited until to be set")
	}
}

func TestAccountPool_ReportSuccess(t *testing.T) {
	accounts := []*config.ProviderAccount{
		{ID: "a1", CredentialType: config.CredentialTypeAPIKey, Credential: "key1", Status: config.AccountStatusError, BackoffLevel: 3, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
	}

	pool := NewAccountPool("test-provider", accounts)

	err := pool.ReportSuccess("a1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	acc := pool.GetAccount("a1")
	if acc.Status != config.AccountStatusActive {
		t.Errorf("expected active status, got %s", acc.Status)
	}
	if acc.BackoffLevel != 0 {
		t.Errorf("expected backoff level 0, got %d", acc.BackoffLevel)
	}
	if !acc.RateLimitedUntil.IsZero() {
		t.Error("expected rate limited until to be zero")
	}
}

func TestAccountPool_ExpiredLocks(t *testing.T) {
	modelLocks := map[string]time.Time{
		"gpt-3.5": time.Now().Add(-1 * time.Hour),
		"gpt-4":   time.Now().Add(1 * time.Hour),
	}
	accounts := []*config.ProviderAccount{
		{ID: "a1", CredentialType: config.CredentialTypeAPIKey, Credential: "key1", Status: config.AccountStatusActive, ModelLocks: modelLocks, CreatedAt: time.Now()},
	}

	pool := NewAccountPool("test-provider", accounts)

	count := pool.ExpiredLocks()
	if count != 1 {
		t.Errorf("expected 1 expired lock, got %d", count)
	}

	acc := pool.GetAccount("a1")
	if _, ok := acc.ModelLocks["gpt-3.5"]; ok {
		t.Error("expected gpt-3.5 lock to be removed")
	}
	if _, ok := acc.ModelLocks["gpt-4"]; !ok {
		t.Error("expected gpt-4 lock to still be present")
	}
}

func TestClassifyError_429(t *testing.T) {
	decision := ClassifyError(429, "rate limited", 0)
	if !decision.ShouldFallback {
		t.Error("expected ShouldFallback for 429")
	}
	if decision.CooldownMs != 1000 {
		t.Errorf("expected 1000ms cooldown, got %d", decision.CooldownMs)
	}
	if decision.NewBackoffLevel != 1 {
		t.Errorf("expected backoff level 1, got %d", decision.NewBackoffLevel)
	}
}

func TestClassifyError_429_MaxBackoff(t *testing.T) {
	decision := ClassifyError(429, "rate limited", 5)
	if decision.NewBackoffLevel != 5 {
		t.Errorf("expected backoff level capped at 5, got %d", decision.NewBackoffLevel)
	}
}

func TestClassifyError_429_InsufficientQuota(t *testing.T) {
	decision := ClassifyError(429, "insufficient_quota: check your billing", 0)
	if !decision.ShouldFallback {
		t.Error("expected ShouldFallback for 429 with quota error")
	}
	if decision.CooldownMs != authErrorCooldownMs {
		t.Errorf("expected auth-length cooldown for quota error, got %d", decision.CooldownMs)
	}
}

func TestClassifyError_500(t *testing.T) {
	decision := ClassifyError(500, "internal error", 0)
	if !decision.ShouldFallback {
		t.Error("expected ShouldFallback for 500")
	}
	if decision.CooldownMs != 5000 {
		t.Errorf("expected 5000ms cooldown, got %d", decision.CooldownMs)
	}
}

func TestClassifyError_401(t *testing.T) {
	decision := ClassifyError(401, "unauthorized", 0)
	if !decision.ShouldFallback {
		t.Error("expected ShouldFallback for 401")
	}
	if decision.CooldownMs != authErrorCooldownMs {
		t.Errorf("expected %dms cooldown, got %d", authErrorCooldownMs, decision.CooldownMs)
	}
}

func TestClassifyError_400(t *testing.T) {
	decision := ClassifyError(400, "bad request", 0)
	if decision.ShouldFallback {
		t.Error("expected no fallback for 400")
	}
}

func TestToProviderAccounts_MultiAccount(t *testing.T) {
	cfg := &config.ProviderConfig{
		Accounts: []config.AccountConfig{
			{ID: "acc1", Type: "apikey", Credential: "key1"},
			{ID: "acc2", Type: "oauth", Credential: "token2"},
		},
	}

	now := time.Now()
	accounts := ToProviderAccountsWithTime(cfg, now)
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	if accounts[0].ID != "acc1" {
		t.Errorf("expected acc1, got %s", accounts[0].ID)
	}
	if accounts[0].CredentialType != config.CredentialTypeAPIKey {
		t.Errorf("expected apikey type, got %s", accounts[0].CredentialType)
	}
	if accounts[1].CredentialType != config.CredentialTypeOAuth {
		t.Errorf("expected oauth type, got %s", accounts[1].CredentialType)
	}
	if !accounts[0].CreatedAt.Equal(now) {
		t.Error("expected CreatedAt to match injected now")
	}
}

func TestToProviderAccounts_LegacyAPIKey(t *testing.T) {
	cfg := &config.ProviderConfig{
		APIKey: "legacy-key",
	}

	accounts := ToProviderAccounts(cfg)
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].ID != "default" {
		t.Errorf("expected default ID, got %s", accounts[0].ID)
	}
	if accounts[0].Credential != "legacy-key" {
		t.Errorf("expected legacy-key, got %s", accounts[0].Credential)
	}
}

func TestToProviderAccounts_NoCredentials(t *testing.T) {
	cfg := &config.ProviderConfig{}
	accounts := ToProviderAccounts(cfg)
	if accounts != nil {
		t.Error("expected nil for no credentials")
	}
}

func TestProviderAccount_String_RedactsCredential(t *testing.T) {
	acc := &config.ProviderAccount{
		ID:             "test",
		CredentialType: config.CredentialTypeAPIKey,
		Credential:     "sk-1234567890abcdef",
		Status:         config.AccountStatusActive,
	}
	s := acc.String()
	if s == "" {
		t.Error("expected non-empty string")
	}
	if s == acc.Credential {
		t.Error("String() should not expose full credential")
	}
}

func TestAccountManager_RegisterPoolAndGetPool(t *testing.T) {
	mgr := NewAccountManager(nil)
	accounts := []*config.ProviderAccount{newTestAccount("a1", "key1")}
	pool := NewAccountPool("test-provider", accounts)
	mgr.RegisterPool("test-provider", pool)

	got, err := mgr.GetPool(context.Background(), "test-provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil pool")
	}
}

func TestAccountManager_GetPool_LazyInit(t *testing.T) {
	mgr := NewAccountManager(nil)
	pool, err := mgr.GetPool(context.Background(), "unknown-provider")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool == nil {
		t.Fatal("expected non-nil pool from lazy init")
	}
}

func TestAccountManager_SelectCredential(t *testing.T) {
	mgr := NewAccountManager(nil)
	accounts := []*config.ProviderAccount{newTestAccount("a1", "my-api-key")}
	pool := NewAccountPool("test-provider", accounts)
	mgr.RegisterPool("test-provider", pool)

	cred, err := mgr.SelectCredential(context.Background(), "test-provider", "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred != "my-api-key" {
		t.Errorf("expected 'my-api-key', got %s", cred)
	}
}

func TestAccountManager_SelectCredential_NoPool(t *testing.T) {
	mgr := NewAccountManager(nil)
	_, err := mgr.SelectCredential(context.Background(), "unknown", "gpt-4")
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestAccountManager_ListProviders(t *testing.T) {
	mgr := NewAccountManager(nil)
	mgr.RegisterPool("p1", NewAccountPool("p1", nil))
	mgr.RegisterPool("p2", NewAccountPool("p2", nil))

	providers := mgr.ListProviders()
	if len(providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(providers))
	}
}

func TestAccountManager_ListAccounts(t *testing.T) {
	mgr := NewAccountManager(nil)
	accounts := []*config.ProviderAccount{newTestAccount("a1", "k1")}
	mgr.RegisterPool("test", NewAccountPool("test", accounts))

	listed := mgr.ListAccounts("test")
	if len(listed) != 1 {
		t.Errorf("expected 1 account, got %d", len(listed))
	}
	if mgr.ListAccounts("nonexistent") != nil {
		t.Error("expected nil for unknown provider")
	}
}

func TestAccountManager_ConcurrentAccess(t *testing.T) {
	mgr := NewAccountManager(nil)
	accounts := []*config.ProviderAccount{
		newTestAccount("a1", "key1"),
		newTestAccount("a2", "key2"),
		newTestAccount("a3", "key3"),
	}
	mgr.RegisterPool("test", NewAccountPool("test", accounts))

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cred, err := mgr.SelectCredential(context.Background(), "test", "gpt-4")
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if cred == "" {
				t.Error("expected non-empty credential")
			}
		}()
	}
	wg.Wait()
}

func TestJSONFileStorage_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	storage := NewJSONFileStorage(dir)

	account := &config.ProviderAccount{
		ID:             "a1",
		CredentialType: config.CredentialTypeAPIKey,
		Credential:     "test-key",
		Status:         config.AccountStatusActive,
		ModelLocks:     make(map[string]time.Time),
		CreatedAt:      time.Now(),
	}

	err := storage.SaveAccount("test-provider", account)
	if err != nil {
		t.Fatalf("SaveAccount error: %v", err)
	}

	loaded, err := storage.LoadAccounts("test-provider")
	if err != nil {
		t.Fatalf("LoadAccounts error: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 account, got %d", len(loaded))
	}
	if loaded[0].ID != "a1" {
		t.Errorf("expected a1, got %s", loaded[0].ID)
	}
	if loaded[0].Credential != "test-key" {
		t.Errorf("expected test-key, got %s", loaded[0].Credential)
	}
}

func TestJSONFileStorage_LoadNonexistent(t *testing.T) {
	dir := t.TempDir()
	storage := NewJSONFileStorage(dir)

	loaded, err := storage.LoadAccounts("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil for nonexistent provider")
	}
}

func TestJSONFileStorage_ListProviders(t *testing.T) {
	dir := t.TempDir()
	storage := NewJSONFileStorage(dir)

	account := &config.ProviderAccount{
		ID: "a1", Credential: "k", Status: config.AccountStatusActive,
		ModelLocks: make(map[string]time.Time), CreatedAt: time.Now(),
	}
	_ = storage.SaveAccount("provider-a", account)
	_ = storage.SaveAccount("provider-b", account)

	providers, err := storage.ListProviders()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(providers))
	}
}

func TestJSONFileStorage_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	storage := NewJSONFileStorage(dir)

	_, err := storage.LoadAccounts("../../etc/passwd")
	if err == nil {
		t.Error("expected error for path traversal provider name")
	}
}

func TestJSONFileStorage_DeleteProvider(t *testing.T) {
	dir := t.TempDir()
	storage := NewJSONFileStorage(dir)

	account := &config.ProviderAccount{
		ID: "a1", Credential: "k", Status: config.AccountStatusActive,
		ModelLocks: make(map[string]time.Time), CreatedAt: time.Now(),
	}
	_ = storage.SaveAccount("test", account)

	err := storage.DeleteProvider("test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	loaded, _ := storage.LoadAccounts("test")
	if loaded != nil {
		t.Error("expected nil after delete")
	}
}
