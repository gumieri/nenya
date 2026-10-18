package adapter

import (
	"sort"
	"strings"
	"sync"
)

// HeaderDroppedContent is the client-visible response header carrying the
// kinds of content a format translation could not represent (NENYA-176).
// The gateway sets it itself; CopyHeaders keeps upstreams from injecting it.
const HeaderDroppedContent = "X-Nenya-Dropped-Content"

// DropReport collects the kinds of content a format translation could not
// represent (opaque reasoning blobs, foreign attachments, unknown block
// types). Nenya's conversions never drop such content silently: they
// substitute a visible placeholder in the translated output and record the
// kind here so the gateway can surface it to the client
// (X-Nenya-Dropped-Content header) and to operators
// (nenya_unsendable_content_total metrics family, NENYA-176).
//
// A report is per-translation (typically per-request); nil-safe accessors
// allow the no-reporting call sites to share the same code path.
type DropReport struct {
	mu    sync.Mutex
	kinds map[string]struct{}
}

// NewDropReport creates an empty DropReport.
func NewDropReport() *DropReport {
	return &DropReport{kinds: make(map[string]struct{})}
}

// Add records that one block of the given kind could not be represented.
// Nil-safe; empty kinds are ignored. Kinds are header-safe and bounded:
// sanitized to [A-Za-z0-9_-] (empty → "unknown"), truncated to the first 48
// bytes, and
// a report retains at most 8 distinct kinds — further kinds collapse into
// "other" — because kinds become metric label values
// (nenya_unsendable_content_total) and the comma-separated
// X-Nenya-Dropped-Content header (NENYA-176); unbounded client-chosen types
// must not create unbounded series.
func (d *DropReport) Add(kind string) {
	if d == nil || kind == "" {
		return
	}
	kind = sanitizeKind(kind)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.kinds == nil {
		d.kinds = make(map[string]struct{})
	}
	if _, dup := d.kinds[kind]; !dup {
		if len(d.kinds) >= maxDropKinds {
			d.kinds["other"] = struct{}{}
			return
		}
		d.kinds[kind] = struct{}{}
	}
}

// maxDropKinds bounds the distinct kinds retained per report; additions
// beyond it collapse into the fixed "other" kind.
const maxDropKinds = 8

// maxDropKindLen bounds a single kind's length in bytes.
const maxDropKindLen = 48

// sanitizeKind makes a block type safe for the comma-separated drop header
// and a bounded label space.
func sanitizeKind(kind string) string {
	if len(kind) > maxDropKindLen {
		kind = kind[:maxDropKindLen]
	}
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}, kind)
	if strings.Trim(cleaned, "_") == "" {
		return "unknown"
	}
	return cleaned
}

// Empty reports whether nothing was recorded. Nil-safe (a nil report counts
// as empty).
func (d *DropReport) Empty() bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.kinds) == 0
}

// Kinds returns the recorded kinds sorted alphabetically. Nil-safe.
func (d *DropReport) Kinds() []string {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.kinds))
	for k := range d.kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
