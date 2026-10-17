package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nenya/internal/gateway"
)

// idempotencyKeyHeader is the client-facing name nenya reads an idempotency
// key from (the Anthropic Messages API convention, NENYA-165). The name a
// PROVIDER receives is per-provider (providers.<name>.idempotency_header).
const idempotencyKeyHeader = "Idempotency-Key"

// idempotencyBindingHeader is gateway-internal bookkeeping kept on the inbound
// request: the digest of the body the current Idempotency-Key is bound to. It
// is never forwarded upstream — only the headers named in buildUpstreamRequest's
// allowlist and the per-provider idempotency header leave the gateway — and the
// chat admission path clears it (resetIdempotencyBinding), so a client cannot
// pin a key to a body of its choosing. On the shared passthrough dispatch paths
// a client-supplied value is not cleared; a forged binding can only make the
// gateway reuse or re-mint a key for that same client's own request (the
// binding carries no cross-request state), which is why it is tolerated there.
const idempotencyBindingHeader = "X-Nenya-Idempotency-Body"

const (
	// idempotencyKeyPrefix marks a gateway-synthesized key, so an operator
	// reading upstream logs can tell it from a client-supplied one.
	idempotencyKeyPrefix = "nenya-"
	// maxIdempotencyKeyLen matches the strictest known provider limit
	// (Anthropic caps the key at 255 characters).
	maxIdempotencyKeyLen = 255
)

// resetIdempotencyBinding clears the internal body binding at admission, so a
// client cannot pin its own key to a body of its choosing by sending the
// binding header. The binding is derived at dispatch from the body that is
// actually sent.
func resetIdempotencyBinding(r *http.Request) {
	if r == nil || r.Header == nil {
		return
	}
	r.Header.Del(idempotencyBindingHeader)
}

// idempotencyKeyForBody returns the idempotency key to present for one
// outbound body. The bool is false only when there is no header map to carry
// the binding (a nil map) — a real request always gets a key: its own, or one
// minted here.
//
// The key is bound to the dispatched BODY, not merely to the client request
// (NENYA-165): a provider that dedups on the key rejects a repeated key whose
// payload differs, so this gateway must not reuse one key for two different
// bodies. Consequences, all deliberate:
//
//   - Every attempt that re-sends an identical body (retry-in-place on a
//     retryable failure, failover between accounts of the same provider)
//     presents the SAME key — the double-billing case the feature exists for.
//   - A re-dispatch whose body differs (an MCP multi-turn iteration appending
//     tool results, a stream continuation, a corrective retry that strips a
//     parameter or summarizes) is re-keyed, because it is a different upstream
//     request.
//   - The client's key is forwarded for the request's first dispatched body,
//     and a synthesized key (fingerprint + per-request nonce) for the rest.
//
// The binding is carried on the inbound header map — the same map every
// dispatch of the request reads — so the state survives across attempts. That
// mutation is safe without a lock only because of a real invariant: every
// dispatch of ONE request runs on its request goroutine (the retry sweep, the
// MCP loop, and the stream continuation are all sequential). Introducing a
// concurrent dispatch of the same request (a hedged/racing request) would
// require moving this state onto a request-scoped struct instead.
//
// The comparison is byte-exact on the dispatched body, so a transform that is
// not deterministic across attempts (a token budget derived from the learned
// calibration ratio, a thought signature restored from the cache) can re-key a
// retry that would otherwise have been deduped. The binding is also a single
// slot, so returning to an earlier body (A -> B -> A) re-keys the second A;
// no path does that today (the sweeps are forward-only, same-target repeats
// only fire at the last dispatchable index, and MCP iterations and stream
// continuations never revisit a body), and the consequence is the safe
// direction either way: a re-key costs at most a lost dedup, never a 4xx for a
// mismatched payload.
func idempotencyKeyForBody(srcHeaders http.Header, body []byte) (string, bool) {
	if srcHeaders == nil {
		return "", false
	}
	digest := idempotencyBodyDigest(body)
	bound := srcHeaders.Get(idempotencyBindingHeader)

	if bound == digest {
		// A repeat of the body this key is already bound to.
		if key, ok := sanitizeIdempotencyKey(srcHeaders.Get(idempotencyKeyHeader)); ok {
			return key, true
		}
	}
	if bound == "" {
		// The request's first dispatch: the client's key, when it is
		// transport-safe, is the key for it.
		if key, ok := sanitizeIdempotencyKey(srcHeaders.Get(idempotencyKeyHeader)); ok {
			srcHeaders.Set(idempotencyKeyHeader, key)
			srcHeaders.Set(idempotencyBindingHeader, digest)
			return key, true
		}
	}

	// No usable client key, or a body the current key is not bound to: mint a
	// key for this body and bind it.
	key := synthesizeIdempotencyKey(digest)
	srcHeaders.Set(idempotencyKeyHeader, key)
	srcHeaders.Set(idempotencyBindingHeader, digest)
	return key, true
}

// idempotencyBodyDigest identifies a dispatched body for key binding. It is
// truncated to 128 bits: a collision would only mean reusing one key for two
// different bodies, and 128 bits puts that beyond reach.
func idempotencyBodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

// sanitizeIdempotencyKey normalizes a client-supplied key for the wire:
// surrounding whitespace is trimmed, and a value that is not visible ASCII
// (a control character, a newline, a non-ASCII byte — none of which a provider
// documents accepting) or that exceeds the length cap is rejected as unusable
// rather than mangled, so the caller can synthesize a safe key instead.
func sanitizeIdempotencyKey(raw string) (string, bool) {
	key := strings.TrimSpace(raw)
	if key == "" || len(key) > maxIdempotencyKeyLen {
		return "", false
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c < 0x21 || c > 0x7e {
			return "", false
		}
	}
	return key, true
}

// synthesizeIdempotencyKey derives a key from the body fingerprint and a
// random nonce: the nonce keeps two byte-identical requests distinct (a
// provider replaying the original response for a repeated key would otherwise
// collapse a legitimate repeat into a dedup), and the fingerprint keeps the
// key derived from the request it belongs to.
func synthesizeIdempotencyKey(fingerprint string) string {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		// crypto/rand is not expected to fail; a key must still be unique per
		// body when it does, so the request clock stands in for the nonce.
		nonce = strconv.AppendInt(nil, time.Now().UnixNano(), 10)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(fingerprint))
	_, _ = digest.Write(nonce)
	return idempotencyKeyPrefix + hex.EncodeToString(digest.Sum(nil)[:16])
}

// providerIdempotencyHeader returns the header name this provider accepts an
// idempotency key on, or "" when the provider declares none — the default:
// the key is forwarded only where the capability is declared (NENYA-165).
func providerIdempotencyHeader(gw *gateway.NenyaGateway, providerName string) string {
	if gw == nil {
		return ""
	}
	provider, ok := gw.Providers[providerName]
	if !ok || provider == nil {
		return ""
	}
	return provider.IdempotencyHeader
}

// injectIdempotencyKey sets the provider's idempotency header on an outbound
// request from the key bound to the body being sent. A provider that declares
// no header, or a request with no usable key, gets none: a provider is never
// handed a malformed key, and a request without one is served exactly as
// before.
func injectIdempotencyKey(gw *gateway.NenyaGateway, providerName string, srcHeaders, dst http.Header, body []byte) {
	header := providerIdempotencyHeader(gw, providerName)
	if header == "" {
		return
	}
	key, ok := idempotencyKeyForBody(srcHeaders, body)
	if !ok {
		return
	}
	dst.Set(header, key)
	if gw.Logger != nil {
		gw.Logger.Debug("forwarding idempotency key", "provider", providerName, "header", header)
	}
}
