// Package billing implements billing-aware routing for Nenya.
//
// It provides three components:
//   - BillingTracker: tracks spend per provider/account and manages exhaustion state
//   - Quota extraction: extracts quota info from JSON responses and HTTP headers
//   - QuotaFetcher: periodic HTTP poller for fetching quota from provider APIs
//
// The QuotaFetcher uses per-provider timeouts read from
// BillingConfig.QuotaTimeoutSeconds (default 10s), enforced via
// context.WithTimeout in the polling goroutines. The shared http.Client
// has Timeout disabled to allow independent per-provider timeout control.
//
// The package supports four billing models (subscription, credit, free, mixed)
// and three quota sources (local tracking, API polling, response headers).
// For mixed providers, free model detection uses a three-tier approach:
// explicit config list, pricing data from discovery catalog, and name heuristic.
package billing
