package auth

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nenya/config"
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

func TestAccountPool_SelectAccountByID_Healthy(t *testing.T) {
	accounts := []*config.ProviderAccount{
		newTestAccount("a1", "key1"),
		newTestAccount("a2", "key2"),
	}
	pool := NewAccountPool("test-provider", accounts)

	acc, err := pool.SelectAccountByID(context.Background(), "a2", "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc.ID != "a2" {
		t.Errorf("expected a2, got %s", acc.ID)
	}
	if acc.Credential != "key2" {
		t.Errorf("expected key2, got %s", acc.Credential)
	}

	// LastUsed is refreshed so LRU selection stays fair for unpinned sessions.
	if pool.GetAccount("a2").LastUsed.IsZero() {
		t.Error("expected LastUsed to be updated after selection")
	}
}

func TestAccountPool_SelectAccountByID_Missing(t *testing.T) {
	pool := NewAccountPool("test-provider", []*config.ProviderAccount{newTestAccount("a1", "key1")})

	_, err := pool.SelectAccountByID(context.Background(), "nope", "gpt-4")
	noAccountErr, ok := err.(*NoAvailableAccountError)
	if !ok {
		t.Fatalf("expected NoAvailableAccountError, got %T", err)
	}
	if noAccountErr.Provider != "test-provider" {
		t.Errorf("expected provider 'test-provider', got %s", noAccountErr.Provider)
	}
	if noAccountErr.Reason != ReasonNotFound {
		t.Errorf("expected reason 'not_found', got %q", noAccountErr.Reason)
	}
}

func TestAccountPool_SelectAccountByID_Cooling(t *testing.T) {
	acc := newTestAccount("a1", "key1")
	acc.RateLimitedUntil = time.Now().Add(1 * time.Hour)
	pool := NewAccountPool("test-provider", []*config.ProviderAccount{acc})

	_, err := pool.SelectAccountByID(context.Background(), "a1", "gpt-4")
	noAccountErr, ok := err.(*NoAvailableAccountError)
	if !ok {
		t.Fatalf("expected NoAvailableAccountError, got %T", err)
	}
	if noAccountErr.Reason != ReasonCooling {
		t.Errorf("expected reason 'cooling', got %q", noAccountErr.Reason)
	}
}

func TestAccountPool_SelectAccountByID_Inactive(t *testing.T) {
	acc := newTestAccount("a1", "key1")
	acc.Status = config.AccountStatusError
	pool := NewAccountPool("test-provider", []*config.ProviderAccount{acc})

	_, err := pool.SelectAccountByID(context.Background(), "a1", "gpt-4")
	noAccountErr, ok := err.(*NoAvailableAccountError)
	if !ok {
		t.Fatalf("expected NoAvailableAccountError, got %T", err)
	}
	if noAccountErr.Reason != ReasonInactive {
		t.Errorf("expected reason 'inactive', got %q", noAccountErr.Reason)
	}
}

func TestAccountPool_SelectAccountByID_ModelLocked(t *testing.T) {
	acc := newTestAccount("a1", "key1")
	acc.ModelLocks["gpt-4"] = time.Now().Add(1 * time.Hour)
	pool := NewAccountPool("test-provider", []*config.ProviderAccount{acc})

	_, err := pool.SelectAccountByID(context.Background(), "a1", "gpt-4")
	noAccountErr, ok := err.(*NoAvailableAccountError)
	if !ok {
		t.Fatalf("expected NoAvailableAccountError, got %T", err)
	}
	if noAccountErr.Reason != ReasonModelLocked {
		t.Errorf("expected reason 'model_locked', got %q", noAccountErr.Reason)
	}
	if _, err := pool.SelectAccountByID(context.Background(), "a1", "claude-3"); err != nil {
		t.Fatalf("unexpected error for unlocked model: %v", err)
	}
}

func TestAccountPool_SelectAccountExcluding(t *testing.T) {
	accounts := []*config.ProviderAccount{
		newTestAccount("a1", "key1"),
		newTestAccount("a2", "key2"),
	}
	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()

	// LRU picks a1 first; excluding it must yield a2.
	first, err := pool.SelectAccount(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := pool.SelectAccountExcluding(ctx, "gpt-4", map[string]bool{first.ID: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("excluded account %s re-selected", first.ID)
	}

	// Excluding every account reports no availability.
	if _, err := pool.SelectAccountExcluding(ctx, "gpt-4", map[string]bool{"a1": true, "a2": true}); err == nil {
		t.Fatal("expected error when all accounts are excluded")
	}
}

func TestAccountManager_SelectAccountByID(t *testing.T) {
	mgr := NewAccountManager(nil)
	mgr.RegisterPool("test-provider", NewAccountPool("test-provider", []*config.ProviderAccount{
		newTestAccount("a1", "key1"),
		newTestAccount("a2", "key2"),
	}))

	acc, err := mgr.SelectAccountByID(context.Background(), "test-provider", "gpt-4", "a2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acc.ID != "a2" || acc.Credential != "key2" {
		t.Errorf("unexpected selection: %+v", acc)
	}

	if _, err := mgr.SelectAccountByID(context.Background(), "test-provider", "gpt-4", "nope"); err == nil {
		t.Error("expected error for unknown account")
	}
	if _, err := mgr.SelectAccountByID(context.Background(), "unknown", "gpt-4", "a1"); err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestAccountPool_SelectAccountByID_Concurrent(t *testing.T) {
	accounts := []*config.ProviderAccount{
		newTestAccount("a1", "key1"),
		newTestAccount("a2", "key2"),
	}
	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if _, err := pool.SelectAccountByID(context.Background(), "a1", "gpt-4"); err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if _, err := pool.SelectAccount(ctx, "gpt-4"); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
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

// TestSelectAccountExcluding_FairUnderFiltering pins the NENYA-40 fairness
// property: with random per-pick exclusions (cooldown/tried-set filtering),
// pick counts across accounts stay nearly equal because selection is
// min-virtual-clock over account identities, never a position within the
// filtered candidate slice. CLIProxyAPI's counter%len(filtered) implementation
// measured a 11331x busiest-to-quietest skew under this workload; the bound
// here is 1.2x.
func TestSelectAccountExcluding_FairUnderFiltering(t *testing.T) {
	const (
		numAccounts  = 9
		numPicks     = 900
		excludeProb  = 0.3
		maxSkewRatio = 1.2
	)
	accounts := make([]*config.ProviderAccount, numAccounts)
	for i := range accounts {
		accounts[i] = newTestAccount(fmt.Sprintf("a%d", i), "key")
	}
	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(42, 7))

	counts := make(map[string]int, numAccounts)
	for i := 0; i < numPicks; i++ {
		exclude := make(map[string]bool, numAccounts)
		for _, acc := range accounts {
			if rng.Float64() < excludeProb {
				exclude[acc.ID] = true
			}
		}
		sel, err := pool.SelectAccountExcluding(ctx, "gpt-4", exclude)
		if err != nil {
			t.Fatalf("pick %d: unexpected error: %v", i, err)
		}
		counts[sel.ID]++
	}

	busiest, quietest := 0, numPicks
	for _, c := range counts {
		if c > busiest {
			busiest = c
		}
		if c < quietest {
			quietest = c
		}
	}
	if quietest == 0 {
		t.Fatalf("starved account under random filtering: counts=%v", counts)
	}
	if ratio := float64(busiest) / float64(quietest); ratio > maxSkewRatio {
		t.Fatalf("busiest/quietest skew %.2fx exceeds %.2fx: counts=%v", ratio, maxSkewRatio, counts)
	}
}

// TestSelectAccountExcluding_WeightedProportion verifies that configured
// weights proportion traffic: a weight-3 account receives ~3x the picks of a
// weight-1 account (weighted fair queuing via clock advance of
// weightTick/weight). The schedule is deterministic, so the counts land close
// to the theoretical optimum.
func TestSelectAccountExcluding_WeightedProportion(t *testing.T) {
	light := newTestAccount("light", "k1")
	light.Weight = 1
	heavy := newTestAccount("heavy", "k2")
	heavy.Weight = 3
	pool := NewAccountPool("test-provider", []*config.ProviderAccount{light, heavy})
	ctx := context.Background()

	const picks = 400
	counts := make(map[string]int, 2)
	for i := 0; i < picks; i++ {
		sel, err := pool.SelectAccount(ctx, "gpt-4")
		if err != nil {
			t.Fatalf("pick %d: unexpected error: %v", i, err)
		}
		counts[sel.ID]++
	}
	if counts["light"] < 90 || counts["light"] > 110 {
		t.Fatalf("weight-1 account got %d/%d picks, want ~100", counts["light"], picks)
	}
	if counts["heavy"] < 270 || counts["heavy"] > 330 {
		t.Fatalf("weight-3 account got %d/%d picks, want ~300", counts["heavy"], picks)
	}
}

// TestSelectAccountExcluding_ZeroWeightDefaultsToOne verifies the documented
// normalization: accounts without an explicit weight (zero value) receive the
// default share, matching weight-1 accounts.
func TestSelectAccountExcluding_ZeroWeightDefaultsToOne(t *testing.T) {
	unset := newTestAccount("unset", "k1")
	one := newTestAccount("one", "k2")
	one.Weight = 1
	pool := NewAccountPool("test-provider", []*config.ProviderAccount{unset, one})
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		if _, err := pool.SelectAccount(ctx, "gpt-4"); err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
	}
	if unset.LastUsed.Sub(one.LastUsed) > weightTick || one.LastUsed.Sub(unset.LastUsed) > weightTick {
		t.Fatalf("zero-weight account advanced differently from weight-1: %v vs %v",
			unset.LastUsed, one.LastUsed)
	}
}

// TestSelectAccountExcluding_ClampsStaleClock verifies that an account whose
// persisted LastUsed is far in the past (e.g. loaded from storage after a
// restart) re-enters selection on equal footing within one tick instead of
// hoarding a backlog of picks proportional to its idle time.
func TestSelectAccountExcluding_ClampsStaleClock(t *testing.T) {
	stale := newTestAccount("stale", "k1")
	stale.LastUsed = time.Now().Add(-72 * time.Hour)
	fresh := newTestAccount("fresh", "k2")
	pool := NewAccountPool("test-provider", []*config.ProviderAccount{fresh, stale})
	ctx := context.Background()

	seen := make(map[string]bool, 2)
	for i := 0; i < 2; i++ {
		sel, err := pool.SelectAccount(ctx, "gpt-4")
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		seen[sel.ID] = true
	}
	if !seen["stale"] || !seen["fresh"] {
		t.Fatalf("stale-clock account did not re-enter within one tick: seen=%v", seen)
	}
}

// TestSelectAccountByID_DoesNotRewindClock verifies sticky picks move the
// account's virtual clock forward (max semantics), never backwards: clocks
// already ahead of wall time — active accounts under sustained traffic — must
// stay ahead so unpinned rotation is not unfairly re-seated.
func TestSelectAccountByID_DoesNotRewindClock(t *testing.T) {
	ahead := newTestAccount("ahead", "k1")
	ahead.LastUsed = time.Now().Add(time.Hour)
	lagging := newTestAccount("lagging", "k2")
	lagging.LastUsed = time.Now().Add(-time.Hour)
	pool := NewAccountPool("test-provider", []*config.ProviderAccount{ahead, lagging})
	ctx := context.Background()

	if _, err := pool.SelectAccountByID(ctx, "ahead", "gpt-4"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ahead.LastUsed.Before(time.Now()) {
		t.Fatal("sticky pick rewound a clock that was ahead of wall time")
	}

	if _, err := pool.SelectAccountByID(ctx, "lagging", "gpt-4"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lagging.LastUsed.Before(time.Now().Add(-weightTick)) {
		t.Fatal("sticky pick did not advance a lagging clock to ~now")
	}
}

// TestAccountPool_ConcurrentSelectionRace exercises concurrent selection and
// error reporting for the race detector (run with -race): the virtual-clock
// advance, clamp, and error classification all mutate shared account state
// under p.mu.
func TestAccountPool_ConcurrentSelectionRace(t *testing.T) {
	accounts := make([]*config.ProviderAccount, 6)
	for i := range accounts {
		accounts[i] = newTestAccount(fmt.Sprintf("a%d", i), "key")
	}
	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()

	var (
		wg     sync.WaitGroup
		served atomic.Int64
	)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 150; i++ {
				sel, err := pool.SelectAccount(ctx, "gpt-4")
				if err != nil {
					continue
				}
				served.Add(1)
				switch (g + i) % 3 {
				case 0:
					_ = pool.ApplyError(sel.ID, 429, "rate limited")
				case 1:
					_ = pool.ApplyError(sel.ID, 500, "boom")
				default:
					_ = pool.ReportSuccess(sel.ID)
				}
			}
		}(g)
	}
	wg.Wait()

	// Restore every account, then verify the pool is serviceable and the
	// recovery path leaves accounts active (§12: no assertion-free tests).
	for _, acc := range accounts {
		_ = pool.ReportSuccess(acc.ID)
	}
	sel, err := pool.SelectAccount(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("pool unserviceable after concurrent load: %v", err)
	}
	if served.Load() == 0 {
		t.Fatal("pool served no selections under concurrent load")
	}
	if acc := pool.GetAccount(sel.ID); acc == nil || acc.Status != config.AccountStatusActive {
		t.Fatalf("selected account not active after recovery (acc=%v)", acc)
	}
}

// TestSelectAccountExcluding_ReentryHoardBounded pins the NENYA-40 review fix:
// sustained pick rates push selection clocks ahead of wall time, and without
// renormalization an account re-entering after a long exclusion would
// monopolize picks proportional to uptime (the pack's accumulated lead) until
// its clock caught up. renormalizeVirtualClocks bounds the hoard to roughly
// one round-robin pass over the pool.
func TestSelectAccountExcluding_ReentryHoardBounded(t *testing.T) {
	const (
		numAccounts = 4
		maxHoard    = numAccounts + 1
	)
	accounts := make([]*config.ProviderAccount, numAccounts)
	for i := range accounts {
		accounts[i] = newTestAccount(fmt.Sprintf("a%d", i), "key")
	}
	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()

	for i := 0; i < 500; i++ {
		if _, err := pool.SelectAccountExcluding(ctx, "gpt-4", map[string]bool{"a0": true}); err != nil {
			t.Fatalf("warmup pick %d: %v", i, err)
		}
	}

	hoarded := 0
	for i := 0; i < 100; i++ {
		sel, err := pool.SelectAccount(ctx, "gpt-4")
		if err != nil {
			t.Fatalf("re-entry pick %d: %v", i, err)
		}
		if sel.ID != "a0" {
			break
		}
		hoarded++
	}
	if hoarded > maxHoard {
		t.Fatalf("re-entering account hoarded %d consecutive picks (max %d) — clock lead unbounded", hoarded, maxHoard)
	}
}

// TestAccountConfigWeightFlowsToPoolSelection verifies the full NENYA-40
// wiring: config accounts[].weight → ToProviderAccounts → AccountPool, with
// the configured proportion visible in actual pick distribution.
func TestAccountConfigWeightFlowsToPoolSelection(t *testing.T) {
	p := &config.ProviderConfig{
		Accounts: []config.AccountConfig{
			{ID: "light", Weight: 1, Type: "apikey", Credential: "c1"},
			{ID: "heavy", Weight: 3, Type: "apikey", Credential: "c2"},
		},
	}
	accounts := ToProviderAccounts(p)
	if accounts[0].Weight != 1 || accounts[1].Weight != 3 {
		t.Fatalf("config weight not copied to pool accounts: %d/%d", accounts[0].Weight, accounts[1].Weight)
	}

	pool := NewAccountPool("test-provider", accounts)
	ctx := context.Background()
	const picks = 400
	counts := make(map[string]int, 2)
	for i := 0; i < picks; i++ {
		sel, err := pool.SelectAccount(ctx, "gpt-4")
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		counts[sel.ID]++
	}
	if counts["light"] < 90 || counts["light"] > 110 {
		t.Fatalf("weight-1 account got %d/%d picks, want ~100", counts["light"], picks)
	}
	if counts["heavy"] < 270 || counts["heavy"] > 330 {
		t.Fatalf("weight-3 account got %d/%d picks, want ~300", counts["heavy"], picks)
	}
}
