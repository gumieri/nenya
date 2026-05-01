package testutil

// TestSecrets returns a minimal set of test tokens.
func TestSecrets() map[string]string {
	return map[string]string{
		"test-admin": "nk-test-admin-secret-token-for-testing",
	}
}
