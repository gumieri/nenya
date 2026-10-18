package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/nenya/config"
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
)

// maxBYOKCredentialLen bounds a caller-supplied credential. Real upstream
// credentials are far shorter; the cap keeps a hostile body from parking
// megabytes in the request headers (NENYA-195).
const maxBYOKCredentialLen = 4096

// applyBYOKCredential resolves a per-request bring-your-own-key override
// (NENYA-195) and applies it to the routing targets.
//
// Contract (fail closed at every step):
//   - the feature must be enabled globally (governance.byok.enabled) AND the
//     presented API key must hold the byok permission (admin, or an explicit
//     permissions.byok = true);
//   - the credential rides ONLY in the configured header — never a query
//     parameter — and must be a bounded, visible-ASCII token (a malformed
//     value is rejected, never silently replaced by the shared key, because
//     that would bill the gateway for a request the caller asked to fund);
//   - only targets whose provider declared providers.<name>.allow_byok are
//     eligible; when a credential is present, the target list is narrowed to
//     those providers (a provider that did not opt in is never handed a
//     caller's credential, and the shared key is never used as a silent
//     fallback for a BYOK request);
//   - no eligible target is a 422 unsupported_combination, not a silent
//     downgrade.
//
// On success every eligible target carries the caller's credential and is
// marked BYOK (so shared-account rotation and breaker state is never written),
// the response cache is bypassed, and the event is audited by API-key name and
// provider — never by credential value.
func (p *Proxy) applyBYOKCredential(gw *gateway.NenyaGateway, r *http.Request, req *chatRequest) *httpError {
	if !req.BYOKRequested {
		return nil // switch off or no credential presented: the shared key serves
	}
	raw := req.byokCredential
	if !auth.KeyMayBYOK(req.apiKey) {
		gw.Metrics.IncAuthDenials(req.KeyRef, "byok")
		return &httpError{
			Code:    http.StatusForbidden,
			Message: "This API key is not authorized to supply an upstream credential",
			Kind:    infra.ErrorKindAuthFailed,
		}
	}
	if !validBYOKCredential(raw) {
		return &httpError{
			Code:    http.StatusBadRequest,
			Message: "Malformed upstream credential",
			Kind:    infra.ErrorKindInvalidRequest,
		}
	}

	eligible := make([]routing.UpstreamTarget, 0, len(req.Targets))
	for _, t := range req.Targets {
		if !providerAllowsBYOK(gw.Providers, t.Provider) {
			continue
		}
		t.Credential = raw
		t.BYOK = true
		eligible = append(eligible, t)
	}
	if len(eligible) == 0 {
		return &httpError{
			Code:    http.StatusUnprocessableEntity,
			Message: "No target provider accepts a caller-supplied credential",
			Kind:    infra.ErrorKindUnsupported,
		}
	}
	req.Targets = eligible
	req.BYOK = true
	// The response cache keys on the payload and policy, not on which
	// credential served: a BYOK request must never be stored or replayed
	// (resolveCache also stands down; clearing here keeps the field honest
	// for any later reader).
	req.CacheKey = ""
	// Audit per distinct provider (a multi-provider agent's chain may span
	// several opt-in providers); the credential value is never recorded.
	seen := make(map[string]struct{}, len(eligible))
	for _, t := range eligible {
		if _, ok := seen[t.Provider]; ok {
			continue
		}
		seen[t.Provider] = struct{}{}
		gw.Metrics.RecordBYOK(t.Provider)
	}
	gw.Logger.Info("byok credential used for request",
		"providers", strings.Join(providerNames(eligible), ","),
		"key", req.KeyRef,
		"targets", len(eligible))
	return nil
}

// extractQuotaUnlessBYOK records the provider's quota headers against the
// shared account, except for a caller-funded dispatch: those headers describe
// the CALLER's credential, and marking the shared account exhausted would take
// the gateway's own traffic down (NENYA-195).
func extractQuotaUnlessBYOK(gw *gateway.NenyaGateway, ctx context.Context, target routing.UpstreamTarget, header http.Header) {
	if target.BYOK {
		return
	}
	gw.ExtractQuotaFromResponseHeaders(ctx, target.Provider, target.AccountName, header)
}

// resolveBYOKIntent records whether this request presents a caller-supplied
// credential: the global switch must be on and the configured header must carry
// a non-empty value. The permission and provider-eligibility gates are applied
// later (applyBYOKCredential), after routing, where the target list exists; the
// intent flag exists so session affinity can stand down before that.
func (p *Proxy) resolveBYOKIntent(gw *gateway.NenyaGateway, r *http.Request, req *chatRequest) {
	cfg := gw.Config.Governance.BYOK
	header := config.DefaultBYOKHeader
	if cfg != nil {
		header = cfg.BYOKHeader()
	}
	value := strings.TrimSpace(r.Header.Get(header))
	if cfg == nil || !cfg.Enabled {
		if value != "" && gw.Logger != nil {
			// A caller that expected its credential to be used deserves a
			// trace of why it was not (the shared key served instead).
			gw.Logger.Debug("byok header presented but governance.byok is disabled; the shared credential served",
				"header", header, "key", req.KeyRef)
		}
		return
	}
	req.byokCredential = value
	req.BYOKRequested = value != ""
}

// providerNames returns the distinct provider names of a target list, in
// first-seen order, for the audit log line.
func providerNames(targets []routing.UpstreamTarget) []string {
	seen := make(map[string]struct{}, len(targets))
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if _, ok := seen[t.Provider]; ok {
			continue
		}
		seen[t.Provider] = struct{}{}
		out = append(out, t.Provider)
	}
	return out
}

// validBYOKCredential reports whether a caller-supplied credential is usable:
// bounded length and visible ASCII only — no control characters, no non-ASCII,
// and no space (0x20), which is what every upstream auth header token carries.
func validBYOKCredential(v string) bool {
	if v == "" || len(v) > maxBYOKCredentialLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}
