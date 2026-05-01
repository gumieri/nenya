package security

import (
	"testing"
)

func TestNewSecureMem(t *testing.T) {
	sm, err := NewSecureMem(4096)
	if err != nil {
		t.Fatalf("NewSecureMem() error = %v", err)
	}
	defer sm.Destroy()

	if sm.size != 4096 {
		t.Errorf("size = %d, want 4096", sm.size)
	}
	if sm.used != 0 {
		t.Errorf("used = %d, want 0", sm.used)
	}
}

func TestStoreAndCompare(t *testing.T) {
	sm, err := NewSecureMem(4096)
	if err != nil {
		t.Fatalf("NewSecureMem() error = %v", err)
	}
	defer sm.Destroy()

	token := GenerateToken()
	st, err := sm.StoreToken(token)
	if err != nil {
		t.Fatalf("StoreToken() error = %v", err)
	}

	if !sm.CompareToken(st, token) {
		t.Error("CompareToken() = false, want true for correct token")
	}

	if sm.CompareToken(st, "wrong-token") {
		t.Error("CompareToken() = true, want false for wrong token")
	}

	if sm.CompareToken(st, "") {
		t.Error("CompareToken() = true, want false for empty token")
	}

	if sm.CompareToken(st, "nk-000000000000000000000000000000000000000000000000") {
		t.Error("CompareToken() = true, want false for token with correct length but wrong content")
	}
}

func TestMultipleTokens(t *testing.T) {
	sm, err := NewSecureMem(8192)
	if err != nil {
		t.Fatalf("NewSecureMem() error = %v", err)
	}
	defer sm.Destroy()

	tokens := []string{GenerateToken(), GenerateToken(), GenerateToken()}
	var refs []SecureToken

	for _, tok := range tokens {
		st, err := sm.StoreToken(tok)
		if err != nil {
			t.Fatalf("StoreToken() error = %v", err)
		}
		refs = append(refs, st)
	}

	for i, tok := range tokens {
		if !sm.CompareToken(refs[i], tok) {
			t.Errorf("CompareToken() token %d = false, want true", i)
		}
	}

	for i := range tokens {
		for j := range tokens {
			if i != j && sm.CompareToken(refs[i], tokens[j]) {
				t.Errorf("CompareToken() token %d matched token %d unexpectedly", i, j)
			}
		}
	}
}

func TestDestroy(t *testing.T) {
	sm, err := NewSecureMem(4096)
	if err != nil {
		t.Fatalf("NewSecureMem() error = %v", err)
	}

	st, err := sm.StoreToken(GenerateToken())
	if err != nil {
		t.Fatalf("StoreToken() error = %v", err)
	}

	sm.Destroy()

	if sm.CompareToken(st, GenerateToken()) {
		t.Error("CompareToken() after Destroy() = true, want false")
	}
}

func TestGenerateToken(t *testing.T) {
	token := GenerateToken()

	if len(token) != 51 {
		t.Errorf("token length = %d, want 51 (3 char prefix + 48 hex)", len(token))
	}

	if token[:3] != "nk-" {
		t.Errorf("token prefix = %q, want nk-", token[:3])
	}

	for i := 3; i < len(token); i++ {
		c := token[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("token contains non-hex char %c at position %d", c, i)
		}
	}

	token2 := GenerateToken()
	if token == token2 {
		t.Error("GenerateToken() produced duplicate tokens")
	}
}

func TestOverflow(t *testing.T) {
	sm, err := NewSecureMem(64)
	if err != nil {
		t.Fatalf("NewSecureMem() error = %v", err)
	}
	defer sm.Destroy()

	_, err = sm.StoreToken("12345678")
	if err != nil {
		t.Fatalf("StoreToken() first error = %v", err)
	}

	_, err = sm.StoreToken("12345678123456781234567812345678123456781234567812345678123456781234567812345678")
	if err == nil {
		t.Error("StoreToken() expected overflow error, got nil")
	}
}

func TestEmptyToken(t *testing.T) {
	sm, err := NewSecureMem(4096)
	if err != nil {
		t.Fatalf("NewSecureMem() error = %v", err)
	}
	defer sm.Destroy()

	_, err = sm.StoreToken("")
	if err == nil {
		t.Error("StoreToken() expected error for empty token, got nil")
	}
}

func TestUsedCounter(t *testing.T) {
	sm, err := NewSecureMem(4096)
	if err != nil {
		t.Fatalf("NewSecureMem() error = %v", err)
	}
	defer sm.Destroy()

	if sm.Used() != 0 {
		t.Errorf("Used() initial = %d, want 0", sm.Used())
	}

	st1, _ := sm.StoreToken("12345678")
	_ = st1
	if sm.Used() == 0 {
		t.Error("Used() after StoreToken = 0, want > 0")
	}
}
