// Package auth handles authentication, authorization, and multi-account
// credential management for the Nenya AI API Gateway.
//
// The primary types are:
//   - AccountManager: top-level coordinator owning per-provider AccountPools
//   - AccountPool: LRU-based account selection with rate-limit and backoff tracking
//   - AccountStorage: persistence interface (JSONFileStorage implementation)
//   - ClassifyError: error classification with exponential backoff decisions
//
// Account selection returns a SelectedAccount value (not a mutable pointer),
// ensuring callers cannot accidentally mutate shared pool state.
package auth
