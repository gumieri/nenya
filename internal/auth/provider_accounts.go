package auth

import (
	"time"

	"nenya/config"
)

// ToProviderAccounts converts a ProviderConfig to a slice of ProviderAccount.
// Supports both legacy single-credential (api_key) and new multi-account (accounts) configurations.
// Returns nil if neither api_key nor accounts are configured.
func ToProviderAccounts(p *config.ProviderConfig) []*config.ProviderAccount {
	return ToProviderAccountsWithTime(p, time.Now())
}

// ToProviderAccountsWithTime is the testable variant that accepts the creation timestamp.
func ToProviderAccountsWithTime(p *config.ProviderConfig, now time.Time) []*config.ProviderAccount {
	if len(p.Accounts) > 0 {
		accounts := make([]*config.ProviderAccount, len(p.Accounts))
		for i, cfg := range p.Accounts {
			var credType config.CredentialType
			switch cfg.Type {
			case "oauth":
				credType = config.CredentialTypeOAuth
			case "cookie":
				credType = config.CredentialTypeCookie
			default:
				credType = config.CredentialTypeAPIKey
			}

			accounts[i] = &config.ProviderAccount{
				ID:             cfg.ID,
				CredentialType: credType,
				Credential:     cfg.Credential,
				Status:         config.AccountStatusActive,
				ModelLocks:     make(map[string]time.Time),
				CreatedAt:      now,
			}
		}
		return accounts
	}

	if p.APIKey != "" {
		return []*config.ProviderAccount{
			{
				ID:             "default",
				CredentialType: config.CredentialTypeAPIKey,
				Credential:     p.APIKey,
				Status:         config.AccountStatusActive,
				ModelLocks:     make(map[string]time.Time),
				CreatedAt:      now,
			},
		}
	}

	return nil
}
