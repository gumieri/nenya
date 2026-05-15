package config

import (
	"fmt"
	"time"
)

// CredentialType represents the type of credential used for authentication.
type CredentialType string

const (
	// CredentialTypeAPIKey uses a static API key for authentication.
	CredentialTypeAPIKey CredentialType = "apikey"
	// CredentialTypeOAuth uses an OAuth token for authentication.
	CredentialTypeOAuth CredentialType = "oauth"
	// CredentialTypeCookie uses a cookie for authentication.
	CredentialTypeCookie CredentialType = "cookie"
)

// AccountStatus represents the current status of a provider account.
type AccountStatus string

const (
	// AccountStatusActive indicates the account is available for use.
	AccountStatusActive AccountStatus = "active"
	// AccountStatusError indicates the account has encountered an error and is on cooldown.
	AccountStatusError AccountStatus = "error"
	// AccountStatusDisabled indicates the account is manually disabled.
	AccountStatusDisabled AccountStatus = "disabled"
)

// ErrorRecord captures details about the last error encountered by an account.
type ErrorRecord struct {
	Status    int       `json:"status"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// ProviderAccount represents a single credential/account for a provider.
// It tracks the credential, status, rate limiting state, and usage metadata.
type ProviderAccount struct {
	ID               string               `json:"id"`
	CredentialType   CredentialType       `json:"credential_type"`
	Credential       string               `json:"credential"`
	Status           AccountStatus        `json:"status"`
	RateLimitedUntil time.Time            `json:"rate_limited_until"`
	LastError        *ErrorRecord         `json:"last_error,omitempty"`
	LastUsed         time.Time            `json:"last_used"`
	ModelLocks       map[string]time.Time `json:"model_locks"`
	BackoffLevel     int                  `json:"backoff_level"`
	CreatedAt        time.Time            `json:"created_at"`
}

// String returns a safe string representation with the credential redacted.
func (a *ProviderAccount) String() string {
	credPreview := ""
	if len(a.Credential) > 8 {
		credPreview = a.Credential[:4] + "..." + a.Credential[len(a.Credential)-4:]
	}
	return fmt.Sprintf("ProviderAccount{id=%s type=%s credential=%q status=%s backoff=%d}",
		a.ID, a.CredentialType, credPreview, a.Status, a.BackoffLevel)
}
