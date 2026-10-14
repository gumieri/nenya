package proxy

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// thoughtSigScope derives the per-caller namespace for upstream-issued
// tool-call-id state (NENYA-189, IDOR class): thought signatures cached while
// serving one API key must not be addressable by another key presenting the
// same upstream-issued id. The key's token is hashed — never stored or logged
// raw; the digest is the full 32 bytes so namespaces cannot collide by
// truncation. The synthetic primary-token key carries an empty token, so
// primary-token traffic shares one namespace: it is a single caller class
// (one operator). ok is false only for a nil key — request paths are always
// authenticated, so callers treat that as fail-closed (no cached state at
// all) rather than falling back to a shared namespace.
func thoughtSigScope(apiKey *config.ApiKey) (scope string, ok bool) {
	if apiKey == nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(apiKey.Token))
	return hex.EncodeToString(sum[:]), true
}

// scopedThoughtSigCache returns the caller-namespaced thought-signature view
// for a request, or an untyped nil when the caller cannot be identified or
// the gateway has no cache — fail closed: a request without an authenticated
// key gets no cached signature state at all, and the nil interface keeps the
// downstream `cache == nil` guards meaningful.
func scopedThoughtSigCache(gw *gateway.NenyaGateway, apiKey *config.ApiKey) infra.ThoughtSignatureKV {
	scope, scoped := thoughtSigScope(apiKey)
	if !scoped || gw == nil || gw.ThoughtSigCache == nil {
		return nil
	}
	return gw.ThoughtSigCache.Scoped(scope)
}
