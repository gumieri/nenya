package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nenya/internal/util"
)

// JSONC-splicing config editor (AUD-45 / NENYA-224).
//
// `nenya config set` used to unmarshal the document into a map and re-marshal
// it, which destroyed every comment the operator wrote and reordered keys (Go
// maps have no order; json.MarshalIndent sorts them). Config files in this
// project are JSONC — the loader strips comments — so comments are
// first-class content: operators annotate credentials, overrides and
// environment caveats.
//
// The editor below never regenerates the document. It descends to the target,
// then performs exactly ONE splice: either replacing the existing value's byte
// span or inserting a new member into its parent object. Every other byte —
// comments, key order, formatting, string escapes — is preserved verbatim.

// jsonScanner tracks enough JSON/JSONC structure to splice safely: string and
// comment state, so braces and commas inside either are ignored.
type jsonScanner struct {
	data []byte
	i    int
}

// skipIgnorable advances past whitespace and // and /* */ comments.
func (s *jsonScanner) skipIgnorable() {
	for s.i < len(s.data) {
		switch s.data[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		case '/':
			if !s.skipComment() {
				return
			}
		default:
			return
		}
	}
}

// skipComment advances past a // or /* */ comment at s.i, reporting whether
// it actually was one.
func (s *jsonScanner) skipComment() bool {
	if s.i+1 >= len(s.data) {
		return false
	}
	switch s.data[s.i+1] {
	case '/':
		for s.i < len(s.data) && s.data[s.i] != '\n' {
			s.i++
		}
		return true
	case '*':
		s.i += 2
		for s.i+1 < len(s.data) && (s.data[s.i] != '*' || s.data[s.i+1] != '/') {
			s.i++
		}
		s.i = min(s.i+2, len(s.data))
		return true
	}
	return false
}

// skipString advances past a JSON string starting at s.i (which must be '"'),
// returning the decoded value when decodable.
func (s *jsonScanner) skipString() (string, bool) {
	start := s.i
	s.i++ // opening quote
	for s.i < len(s.data) {
		switch s.data[s.i] {
		case '\\':
			s.i += 2
			continue
		case '"':
			s.i++
			var decoded string
			if err := json.Unmarshal(s.data[start:s.i], &decoded); err != nil {
				return "", false
			}
			return decoded, true
		}
		s.i++
	}
	return "", false
}

// skipValue advances past one complete JSON value, returning its byte span.
func (s *jsonScanner) skipValue() (start, end int, ok bool) {
	s.skipIgnorable()
	if s.i >= len(s.data) {
		return 0, 0, false
	}
	start = s.i
	switch s.data[s.i] {
	case '"':
		if _, ok := s.skipString(); !ok {
			return 0, 0, false
		}
		return start, s.i, true
	case '{', '[':
		return s.skipComposite()
	default:
		for s.i < len(s.data) {
			switch s.data[s.i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return start, s.i, s.i > start
			}
			s.i++
		}
		return start, s.i, s.i > start
	}
}

// skipComposite advances past a balanced object or array, ignoring structural
// characters inside strings and comments.
func (s *jsonScanner) skipComposite() (start, end int, ok bool) {
	start = s.i
	open := s.data[s.i]
	closing := byte('}')
	if open == '[' {
		closing = ']'
	}
	depth := 0
	for s.i < len(s.data) {
		switch s.data[s.i] {
		case '"':
			if _, ok := s.skipString(); !ok {
				return 0, 0, false
			}
			continue
		case '/':
			if s.skipComment() {
				continue
			}
		case open:
			depth++
		case closing:
			depth--
			if depth == 0 {
				s.i++
				return start, s.i, true
			}
		}
		s.i++
	}
	return 0, 0, false
}

// objectKeySpan locates key inside the object whose body starts at bodyStart
// (the offset just after its '{').
func objectKeySpan(data []byte, bodyStart int, key string) (valStart, valEnd int, ok bool) {
	s := &jsonScanner{data: data, i: bodyStart}
	for {
		s.skipIgnorable()
		if s.i >= len(data) || data[s.i] == '}' {
			return 0, 0, false
		}
		if data[s.i] == ',' {
			s.i++
			continue
		}
		if data[s.i] != '"' {
			return 0, 0, false
		}
		gotKey, keyOK := s.skipString()
		if !keyOK {
			return 0, 0, false
		}
		s.skipIgnorable()
		if s.i >= len(data) || data[s.i] != ':' {
			return 0, 0, false
		}
		s.i++
		vs, ve, valueOK := s.skipValue()
		if !valueOK {
			return 0, 0, false
		}
		if gotKey == key {
			return vs, ve, true
		}
	}
}

// objectInsertOffset returns where a new member may be inserted in the object
// whose body starts at bodyStart, and whether the object is empty (no leading
// comma needed). Comments that trail the last value stay attached to it.
func objectInsertOffset(data []byte, bodyStart int) (offset int, empty bool, ok bool) {
	s := &jsonScanner{data: data, i: bodyStart}
	lastEnd := -1
	for {
		s.skipIgnorable()
		if s.i >= len(data) {
			return 0, false, false
		}
		if data[s.i] == '}' {
			if lastEnd < 0 {
				return s.i, true, true
			}
			return lastEnd, false, true
		}
		if data[s.i] == ',' {
			s.i++
			continue
		}
		_, ve, valueOK := s.skipValue()
		if !valueOK {
			return 0, false, false
		}
		lastEnd = ve
	}
}

// detectIndentUnit returns the indentation step used by the document, so an
// inserted member matches the surrounding style.
func detectIndentUnit(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		return line[:len(line)-len(trimmed)]
	}
	return "  "
}

// spliceConfigKey sets dottedKey to value inside the JSONC document,
// preserving every other byte. An empty document is built from scratch.
func spliceConfigKey(doc []byte, dottedKey string, value any) ([]byte, error) {
	parts := strings.Split(dottedKey, ".")
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("invalid config key %q", dottedKey)
		}
	}

	indentUnit := detectIndentUnit(doc)
	encoded, err := json.MarshalIndent(value, "", indentUnit)
	if err != nil {
		return nil, fmt.Errorf("encode value: %w", err)
	}
	if len(bytes.TrimSpace(doc)) == 0 {
		return buildNestedDocument(parts, encoded, indentUnit), nil
	}

	root := &jsonScanner{data: doc}
	root.skipIgnorable()
	if root.i >= len(doc) || doc[root.i] != '{' {
		return nil, fmt.Errorf("document root is not a JSON object")
	}
	bodyStart := root.i + 1

	// Descend the existing object chain. Every lookup is done on the ORIGINAL
	// bytes, so the single splice below is computed against stable offsets.
	for depth, part := range parts {
		vs, ve, found := objectKeySpan(doc, bodyStart, part)

		if !found {
			// Create the missing tail inside this object. nestRemaining
			// returns the complete "key": value member text (nested when the
			// remaining path has more than one component).
			tail, err := nestRemaining(parts[depth:], encoded, indentUnit, depth)
			if err != nil {
				return nil, err
			}
			return insertMember(doc, bodyStart, tail, depth, indentUnit)
		}

		if depth == len(parts)-1 {
			// Existing leaf: replace exactly its span.
			// util.AddCap: CodeQL flags the capacity sum as a potential
			// overflow; the subtraction above cannot underflow (vs..ve is a
			// span within doc), the addition is clamped.
			out := make([]byte, 0, util.AddCap(len(doc)-(ve-vs), len(encoded)))
			out = append(out, doc[:vs]...)
			out = append(out, encoded...)
			out = append(out, doc[ve:]...)
			return out, nil
		}

		inner := &jsonScanner{data: doc, i: vs}
		inner.skipIgnorable()
		if inner.i >= len(doc) || doc[inner.i] != '{' {
			return nil, fmt.Errorf("config key %q is not an object", strings.Join(parts[:depth+1], "."))
		}
		bodyStart = inner.i + 1
	}

	return nil, fmt.Errorf("internal: unreachable for %q", dottedKey)
}

// nestRemaining renders parts[0..] as a complete `"key": value` member text,
// nesting further objects when more than one component remains.
func nestRemaining(parts []string, leaf []byte, indentUnit string, depth int) ([]byte, error) {
	last := len(parts) - 1
	keyJSON, err := json.Marshal(parts[last])
	if err != nil {
		return nil, err
	}
	out := string(keyJSON) + ": " + string(leaf)
	for i := last - 1; i >= 0; i-- {
		keyJSON, err := json.Marshal(parts[i])
		if err != nil {
			return nil, err
		}
		innerIndent := strings.Repeat(indentUnit, depth+i+2)
		closeIndent := strings.Repeat(indentUnit, depth+i+1)
		out = string(keyJSON) + ": {\n" + innerIndent + out + "\n" + closeIndent + "}"
	}
	return []byte(out), nil
}

// insertMember splices member into the object at bodyStart.
func insertMember(doc []byte, bodyStart int, member []byte, depth int, indentUnit string) ([]byte, error) {
	offset, empty, ok := objectInsertOffset(doc, bodyStart)
	if !ok {
		return nil, fmt.Errorf("could not locate an insertion point")
	}
	separator := ""
	if !empty {
		separator = ","
	}
	insertion := separator + "\n" + strings.Repeat(indentUnit, depth+1) + string(member)
	out := make([]byte, 0, util.AddCap(len(doc), len(insertion)))
	out = append(out, doc[:offset]...)
	out = append(out, insertion...)
	out = append(out, doc[offset:]...)
	return out, nil
}

// buildNestedDocument renders parts as nested objects around leaf.
func buildNestedDocument(parts []string, leaf []byte, indentUnit string) []byte {
	out := string(leaf)
	for i := len(parts) - 1; i >= 0; i-- {
		keyJSON, _ := json.Marshal(parts[i])
		innerIndent := strings.Repeat(indentUnit, i+1)
		out = "{\n" + innerIndent + string(keyJSON) + ": " + out + "\n" + strings.Repeat(indentUnit, i) + "}"
	}
	return []byte(out + "\n")
}
