package tracing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// TraceID and SpanID are the W3C Trace Context identifiers (16 and 8 bytes).
type TraceID [16]byte

// SpanID identifies one span within a trace.
type SpanID [8]byte

func (id TraceID) String() string { return hex.EncodeToString(id[:]) }
func (id SpanID) String() string  { return hex.EncodeToString(id[:]) }

const (
	traceparentVersion = "00"
	sampledFlag        = 0x01
	// traceparentLen is the fixed W3C wire length: 2 + 1 + 32 + 1 + 16 + 1 + 2.
	traceparentLen = 2 + 1 + 32 + 1 + 16 + 1 + 2
)

// SpanContext is the immutable identity of a span on the wire.
type SpanContext struct {
	TraceID TraceID
	SpanID  SpanID
	Sampled bool
}

// ParseTraceparent parses a W3C traceparent header value per the Trace
// Context specification: version 00 is exactly 55 chars; higher versions
// are parsed at the fixed offsets with extension bytes ignored (forward
// compatibility, §3.2.4); the forbidden version ff and invalid ids are
// rejected. Deliberately lenient on input normalization (leading/trailing
// whitespace trimmed, uppercase hex accepted) — fail-open for a proxy.
func ParseTraceparent(raw string) (SpanContext, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < traceparentLen || raw[2] != '-' || raw[35] != '-' || raw[52] != '-' {
		return SpanContext{}, false
	}
	version, err := hex.DecodeString(raw[0:2])
	if err != nil || version[0] == 0xff {
		return SpanContext{}, false
	}
	if version[0] == 0 && len(raw) != traceparentLen {
		// Version 00 has no extension fields: the spec fixes the length.
		return SpanContext{}, false
	}
	var sc SpanContext
	traceBytes, err := hex.DecodeString(raw[3:35])
	if err != nil {
		return SpanContext{}, false
	}
	copy(sc.TraceID[:], traceBytes)
	spanBytes, err := hex.DecodeString(raw[36:52])
	if err != nil {
		return SpanContext{}, false
	}
	copy(sc.SpanID[:], spanBytes)
	flags, err := strconv.ParseUint(raw[53:55], 16, 8)
	if err != nil {
		return SpanContext{}, false
	}
	sc.Sampled = flags&sampledFlag != 0
	if sc.TraceID == (TraceID{}) || sc.SpanID == (SpanID{}) {
		return SpanContext{}, false
	}
	return sc, true
}

// FormatTraceparent renders the canonical wire form for a span context.
func FormatTraceparent(sc SpanContext) string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return fmt.Sprintf("%s-%s-%s-%s", traceparentVersion, sc.TraceID, sc.SpanID, flags)
}

// NewIDs generates a fresh trace/span pair (crypto/rand; the source of all
// identifier entropy in this package).
func NewIDs() (TraceID, SpanID) {
	var id TraceID
	var span SpanID
	_, _ = rand.Read(id[:])
	_, _ = rand.Read(span[:])
	// Per W3C, trace/span ids must not be all-zero.
	id[0] |= 0x01
	span[0] |= 0x01
	return id, span
}
