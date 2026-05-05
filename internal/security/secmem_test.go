package security

import (
	"strings"
	"testing"
)

func TestNewSecureMem_InvalidCapacity(t *testing.T) {
	_, err := NewSecureMem(0)
	if err == nil {
		t.Error("expected error for zero capacity")
	}
}

func TestNewSecureMem_NegativeCapacity(t *testing.T) {
	_, err := NewSecureMem(-1)
	if err == nil {
		t.Error("expected error for negative capacity")
	}
}

func TestSecureMem_StoreAndCompare(t *testing.T) {
	sm, err := NewSecureMem(4096)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	st, err := sm.StoreToken("sk-test-api-key-12345")
	if err != nil {
		t.Fatalf("StoreToken failed: %v", err)
	}

	if !sm.CompareToken(st, "sk-test-api-key-12345") {
		t.Error("CompareToken returned false for correct token")
	}
	if sm.CompareToken(st, "wrong-key") {
		t.Error("CompareToken returned true for wrong token")
	}
}

func TestSecureMem_CompareToken_Empty(t *testing.T) {
	sm, err := NewSecureMem(256)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	st, err := sm.StoreToken("test-key")
	if err != nil {
		t.Fatalf("StoreToken failed: %v", err)
	}

	if sm.CompareToken(SecureToken{}, "") {
		t.Error("CompareToken returned true for empty SecureToken")
	}
	if sm.CompareToken(st, "") {
		t.Error("CompareToken returned true for empty input")
	}
}

func TestSecureMem_GetToken(t *testing.T) {
	sm, err := NewSecureMem(256)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	st, err := sm.StoreToken("my-secret-key")
	if err != nil {
		t.Fatalf("StoreToken failed: %v", err)
	}

	token, ok := sm.GetToken(st)
	if !ok {
		t.Fatal("GetToken returned false")
	}
	if string(token) != "my-secret-key" {
		t.Errorf("expected 'my-secret-key', got %q", string(token))
	}
}

func TestSecureMem_GetToken_Invalid(t *testing.T) {
	sm, err := NewSecureMem(256)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	_, ok := sm.GetToken(SecureToken{offset: -1, length: 5})
	if ok {
		t.Error("expected false for invalid token")
	}

	_, ok = sm.GetToken(SecureToken{offset: 0, length: 0})
	if ok {
		t.Error("expected false for zero-length token")
	}
}

func TestSecureMem_StoreToken_Empty(t *testing.T) {
	sm, err := NewSecureMem(256)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	_, err = sm.StoreToken("")
	if err == nil {
		t.Error("expected error for empty token")
	}
}

func TestSecureMem_StoreToken_RespectsAlignment(t *testing.T) {
	sm, err := NewSecureMem(64)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	st, err := sm.StoreToken("a")
	if err != nil {
		t.Fatalf("StoreToken failed: %v", err)
	}
	if st.offset != 0 {
		t.Errorf("expected offset 0, got %d", st.offset)
	}
	used := sm.Used()
	if used != 8 {
		t.Errorf("expected used=8 (aligned), got %d", used)
	}
}

func TestSecureMem_StoreToken_Overflow(t *testing.T) {
	sm, err := NewSecureMem(8)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	_, err = sm.StoreToken("very-long-token-that-exceeds-capacity")
	if err == nil {
		t.Error("expected error for overflow")
	}
}

func TestSecureMem_Seal(t *testing.T) {
	sm, err := NewSecureMem(256)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	st, err := sm.StoreToken("secret")
	if err != nil {
		t.Fatalf("StoreToken failed: %v", err)
	}

	if err := sm.Seal(); err != nil {
		t.Fatalf("Seal failed: %v", err)
	}

	_, err = sm.StoreToken("another-secret")
	if err == nil {
		t.Error("expected error when storing in sealed memory")
	}

	if !sm.CompareToken(st, "secret") {
		t.Error("CompareToken should still work after seal")
	}
}

func TestSecureMem_Seal_Nil(t *testing.T) {
	var sm *SecureMem
	if err := sm.Seal(); err != nil {
		t.Errorf("expected nil error for nil receiver, got: %v", err)
	}
}

func TestSecureMem_Seal_Twice(t *testing.T) {
	sm, err := NewSecureMem(256)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	if err := sm.Seal(); err != nil {
		t.Fatalf("first Seal failed: %v", err)
	}

	if err := sm.Seal(); err != nil {
		t.Errorf("second Seal should be idempotent, got: %v", err)
	}
}

func TestSecureMem_Destroy(t *testing.T) {
	sm, err := NewSecureMem(256)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}

	_, err = sm.StoreToken("secret-data")
	if err != nil {
		t.Fatalf("StoreToken failed: %v", err)
	}

	sm.Destroy()

	if sm.Used() != 0 {
		t.Error("expected Used()=0 after destroy")
	}

	sm.Destroy()
}

func TestSecureMem_Destroy_Nil(t *testing.T) {
	var sm *SecureMem
	sm.Destroy()
}

func TestSecureMem_Used(t *testing.T) {
	sm, err := NewSecureMem(256)
	if err != nil {
		t.Fatalf("NewSecureMem failed: %v", err)
	}
	defer sm.Destroy()

	if used := sm.Used(); used != 0 {
		t.Errorf("expected 0 used initially, got %d", used)
	}

	_, err = sm.StoreToken("hello")
	if err != nil {
		t.Fatalf("StoreToken failed: %v", err)
	}

	if used := sm.Used(); used == 0 {
		t.Error("expected non-zero used after store")
	}
}

func TestGenerateToken(t *testing.T) {
	token := GenerateToken()
	if !strings.HasPrefix(token, "nk-") {
		t.Errorf("expected token to start with 'nk-', got %q", token)
	}
	if len(token) != 51 {
		t.Errorf("expected token length 51, got %d: %q", len(token), token)
	}
}

func TestGenerateToken_Unique(t *testing.T) {
	t1 := GenerateToken()
	t2 := GenerateToken()
	if t1 == t2 {
		t.Error("expected unique tokens")
	}
}

func TestTokenSizeHint(t *testing.T) {
	tests := []struct {
		name            string
		numKeys         int
		providerKeyCount int
		want            int
	}{
		{"zero_keys", 0, 0, 0},
		{"single_key", 1, 0, 72},
		{"multiple_keys", 5, 3, 576},
		{"negative_num", -1, 0, 0},
		{"negative_provider", 0, -1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TokenSizeHint(tt.numKeys, tt.providerKeyCount)
			if got != tt.want {
				t.Errorf("TokenSizeHint(%d, %d) = %d, want %d", tt.numKeys, tt.providerKeyCount, got, tt.want)
			}
		})
	}
}

func TestTokenSizeHint_Overflow(t *testing.T) {
	result := TokenSizeHint(1<<60, 1<<60)
	if result < 0 {
		t.Error("expected non-negative result on overflow clamp")
	}
}
