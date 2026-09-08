package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// anthropicUnifiedResetHeaders are the Anthropic unified rate-limit window
// reset headers (NENYA-43). They carry RFC3339 timestamps for the rolling
// 5h and 7d usage windows; on a 429 the longest applicable window is the
// cooldown the upstream actually intends. http.Header.Get canonicalizes the
// key, so case variations from proxies are handled.
var anthropicUnifiedResetHeaders = []string{
	"Anthropic-Ratelimit-Unified-5h-Reset",
	"Anthropic-Ratelimit-Unified-7d-Reset",
	"X-Ratelimit-Reset",
}

// epochOrDelaySeconds coerces a bare numeric reset/delay value (NENYA-43):
// values ≥1e11 are unix milliseconds, values >1e9 are unix epoch seconds,
// and smaller values are plain seconds-from-now (delay semantics). Past
// timestamps yield 0.
func epochOrDelaySeconds(v float64) time.Duration {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	switch {
	case v >= 1e12: // unix milliseconds
		if d := time.Until(time.UnixMilli(int64(v))); d > 0 {
			return d
		}
	case v >= 1e11: // unix milliseconds in the past
		return 0
	case v > 1e9: // unix epoch seconds (post-2001 sanity bound)
		if d := time.Until(time.Unix(int64(v), 0)); d > 0 {
			return d
		}
	default: // plain seconds-from-now (delay semantics)
		return time.Duration(v * float64(time.Second))
	}
	return 0
}

// parseHeaderResetDuration parses a rate-limit reset header value: an RFC3339
// timestamp, a bare unix-epoch or delay-seconds number, or a Go-style
// duration ("90s", "6m0s"). Returns 0 for empty, past, or unparseable values.
func parseHeaderResetDuration(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if ts, err := time.Parse(time.RFC3339, v); err == nil {
		if d := time.Until(ts); d > 0 {
			return d
		}
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return epochOrDelaySeconds(secs)
	}
	// Go durations lack day units; normalize an "Nd" suffix (OpenAI-style
	// x-ratelimit-reset values) before parsing.
	if n := len(v); n > 1 && v[n-1] == 'd' {
		if days, err := strconv.ParseFloat(v[:n-1], 64); err == nil && days > 0 {
			return time.Duration(days * 24 * float64(time.Hour))
		}
	}
	if dur, err := time.ParseDuration(v); err == nil && dur > 0 {
		return dur
	}
	return 0
}

// maxRateLimitWindowCooldown bounds derived rate-limit cooldowns (NENYA-43).
// Anthropic's longest unified window is 7d, so the cap sits just above it —
// tight enough to clamp garbage far-future timestamps, loose enough to honor
// real weekly windows.
const maxRateLimitWindowCooldown = 8 * 24 * time.Hour

// parseRetryAfterCooldown extracts the Retry-After cooldown: seconds, a Go
// duration, or an HTTP-date.
func parseRetryAfterCooldown(header http.Header) time.Duration {
	v := header.Get("Retry-After")
	if v == "" {
		return 0
	}
	longest := parseHeaderResetDuration(v)
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > longest {
			longest = d
		}
	}
	return longest
}

// parseResetValue coerces a reset-field value into a duration from now:
// unix milliseconds, unix epoch seconds, plain seconds-from-now, RFC3339, or
// a duration string. Returns 0 for unsupported shapes and past timestamps.
func parseResetValue(val interface{}) time.Duration {
	switch v := val.(type) {
	case float64:
		return epochOrDelaySeconds(v)
	case string:
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			if d := time.Until(ts); d > 0 {
				return d
			}
			return 0
		}
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			return epochOrDelaySeconds(secs)
		}
		if dur, err := time.ParseDuration(v); err == nil && dur > 0 {
			return dur
		}
	}
	return 0
}

// deriveUpstreamRateLimitCooldown computes the cooldown an upstream 429
// actually asks for (NENYA-43): the longest applicable window among
// Retry-After, the Anthropic unified rate-limit window resets (5h/7d), and
// the generic X-Ratelimit-Reset. Only future reset points count; capped at
// maxRateLimitWindowCooldown. Returns 0 when no signal is present.
func deriveUpstreamRateLimitCooldown(header http.Header) time.Duration {
	if header == nil {
		return 0
	}
	longest := parseRetryAfterCooldown(header)
	for _, name := range anthropicUnifiedResetHeaders {
		if d := parseHeaderResetDuration(header.Get(name)); d > longest {
			longest = d
		}
	}
	if longest > maxRateLimitWindowCooldown {
		longest = maxRateLimitWindowCooldown
	}
	return longest
}

// parseQuotaResetFields scans a quota error body for reset-timestamp fields —
// top-level, nested under "error", or "error"."metadata" — with keys matched
// case-insensitively containing "reset" (Codex/OpenRouter-style usage-limit
// shapes, NENYA-43). Values may be unix milliseconds, unix seconds, RFC3339
// strings, or plain durations. Returns the longest future reset, or 0.
// Known limitations (accepted, cap-bounded): deeper nesting (e.g.
// error.rate_limit.reset_at) and error ARRAYS are not descended, and any
// numeric field whose key merely contains "reset" (e.g. resets_remaining)
// is treated as a cooldown.
func parseQuotaResetFields(body []byte) time.Duration {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return 0
	}

	longest := time.Duration(0)
	var walk func(m map[string]interface{})
	walk = func(m map[string]interface{}) {
		for k, v := range m {
			if strings.Contains(strings.ToLower(k), "reset") {
				if d := parseResetValue(v); d > longest {
					longest = d
				}
			}
			if sub, ok := v.(map[string]interface{}); ok &&
				(strings.EqualFold(k, "error") || strings.EqualFold(k, "metadata")) {
				walk(sub)
			}
		}
	}
	walk(root)

	if longest > maxRateLimitWindowCooldown {
		longest = maxRateLimitWindowCooldown
	}
	return longest
}
