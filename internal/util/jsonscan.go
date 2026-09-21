package util

import (
	"bytes"
	"encoding/json"
)

// ExtractTopLevelField scans a JSON object for one top-level field without
// unmarshaling the document (NENYA-35). The hot path needs a single field
// (e.g. "type" for format detection); a full unmarshal materializes every
// nested value just to read one.
//
// Implementation is a hand-rolled byte scanner: string-aware depth
// counting to skip non-matching values, zero-copy json.RawMessage capture
// of the matching value, and no allocations on miss. Presence semantics
// match json.Unmarshal into map[string]interface{}:
//   - duplicate keys: the LAST occurrence wins (scanning continues)
//   - non-object top level (arrays, scalars): not found
//   - malformed or truncated JSON, trailing garbage, BOM: not found
//
// Keys containing backslash escapes are unescaped via encoding/json before
// comparison; plain keys are compared byte-for-byte.
func ExtractTopLevelField(body []byte, field string) (json.RawMessage, bool) {
	i := skipWS(body, 0)
	if i >= len(body) || body[i] != '{' {
		return nil, false
	}
	i = skipWS(body, i+1)
	if i < len(body) && body[i] == '}' {
		// Empty object: valid, nothing to find.
		if onlyWS(body, i+1) {
			return nil, false
		}
		return nil, false
	}

	return scanObjectEntries(body, i, field)
}

// scanObjectEntries walks the entries of an object after '{', capturing the
// last occurrence of the requested field.
func scanObjectEntries(body []byte, i int, field string) (json.RawMessage, bool) {
	var found json.RawMessage
	have := false

	for {
		i = skipWS(body, i)
		if i >= len(body) || body[i] != '"' {
			return nil, false
		}
		keyStart := i
		keyEnd, ok := skipString(body, i)
		if !ok {
			return nil, false
		}
		i = skipWS(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return nil, false
		}
		i = skipWS(body, i+1)

		valStart := i
		i, ok = skipValue(body, i)
		if !ok {
			return nil, false
		}

		if keyMatches(body[keyStart:keyEnd], field) {
			found = json.RawMessage(body[valStart:i])
			have = true
		}

		i = skipWS(body, i)
		if i >= len(body) {
			return nil, false // truncated
		}
		switch body[i] {
		case ',':
			i = skipWS(body, i+1)
			if i < len(body) && body[i] == '}' {
				return nil, false // trailing comma is invalid JSON
			}
		case '}':
			if !onlyWS(body, i+1) {
				return nil, false // trailing garbage
			}
			return found, have
		default:
			return nil, false
		}
	}
}

func keyMatches(rawKey []byte, field string) bool {
	// Fast path: unquoted-escape-free key compared as bytes. rawKey still
	// includes the surrounding quotes.
	if !bytes.ContainsRune(rawKey, '\\') {
		return len(rawKey) == len(field)+2 && string(rawKey[1:len(rawKey)-1]) == field
	}
	var unquoted string
	if err := json.Unmarshal(rawKey, &unquoted); err != nil {
		return false
	}
	return unquoted == field
}

func skipWS(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func onlyWS(b []byte, i int) bool {
	return skipWS(b, i) == len(b)
}

// skipString consumes a JSON string starting at the opening quote,
// enforcing RFC 8259 string rules: raw control bytes (< 0x20) are invalid,
// and backslash escapes must be one of \" \\ \/ b f n r t u (with \u
// followed by four hex digits).
func skipString(b []byte, i int) (int, bool) {
	i++ // opening quote
	for i < len(b) {
		c := b[i]
		switch {
		case c == '"':
			return i + 1, true
		case c == '\\':
			if i+1 >= len(b) {
				return 0, false
			}
			switch b[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if i+6 > len(b) || !isHex4(b[i+2:i+6]) {
					return 0, false
				}
				i += 6
			default:
				return 0, false
			}
		case c < 0x20:
			return 0, false // raw control byte
		default:
			i++
		}
	}
	return 0, false
}

func isHex4(b []byte) bool {
	for _, c := range b {
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// skipValue consumes and validates any JSON value (object, array, string,
// number, true/false/null) starting at i. Structural validation is required
// for oracle parity with json.Unmarshal: a document containing malformed
// nested containers must not yield a "found" result.
func skipValue(b []byte, i int) (int, bool) {
	if i >= len(b) {
		return 0, false
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{':
		return skipObject(b, i)
	case '[':
		return skipArray(b, i)
	case 't':
		return expectLiteral(b, i, "true")
	case 'f':
		return expectLiteral(b, i, "false")
	case 'n':
		return expectLiteral(b, i, "null")
	default:
		return skipNumber(b, i)
	}
}

func expectLiteral(b []byte, i int, lit string) (int, bool) {
	if i+len(lit) <= len(b) && string(b[i:i+len(lit)]) == lit {
		return i + len(lit), true
	}
	return 0, false
}

// skipNumber consumes a JSON number per RFC 8259: optional minus, integer
// part, optional fraction, optional exponent.
func skipNumber(b []byte, i int) (int, bool) {
	if i < len(b) && b[i] == '-' {
		i++
	}
	intStart := i
	i, intDigits, ok := skipDigits(b, i)
	if !ok {
		return 0, false
	}
	// RFC 8259: no leading zeros ("01" is invalid; "0" alone is fine).
	// intStart is the first digit (past the minus, if any).
	if intDigits > 1 && b[intStart] == '0' {
		return 0, false
	}
	if i < len(b) && b[i] == '.' {
		i++
		frac := 0
		for i < len(b) && isDigit(b[i]) {
			i++
			frac++
		}
		if frac == 0 {
			return 0, false
		}
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		return skipExponent(b, i)
	}
	return i, true
}

// skipDigits consumes a run of ASCII digits, returning the new offset and
// the digit count.
func skipDigits(b []byte, i int) (int, int, bool) {
	digits := 0
	for i < len(b) && isDigit(b[i]) {
		i++
		digits++
	}
	return i, digits, digits > 0
}

// skipExponent consumes the e[+-]ddd exponent tail of a number.
func skipExponent(b []byte, i int) (int, bool) {
	i++ // 'e' or 'E'
	if i < len(b) && (b[i] == '+' || b[i] == '-') {
		i++
	}
	exp := 0
	for i < len(b) && isDigit(b[i]) {
		i++
		exp++
	}
	if exp == 0 {
		return 0, false
	}
	return i, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// skipObject consumes a JSON object: '{' (ws string ':' value (',' ws
// string ':' value)*)? '}'.
func skipObject(b []byte, i int) (int, bool) {
	i++ // '{'
	i = skipWS(b, i)
	if i < len(b) && b[i] == '}' {
		return i + 1, true
	}
	for {
		i = skipWS(b, i)
		if i >= len(b) || b[i] != '"' {
			return 0, false
		}
		var ok bool
		if i, ok = skipString(b, i); !ok {
			return 0, false
		}
		i = skipWS(b, i)
		if i >= len(b) || b[i] != ':' {
			return 0, false
		}
		i = skipWS(b, i+1)
		if i, ok = skipValue(b, i); !ok {
			return 0, false
		}
		i = skipWS(b, i)
		if i >= len(b) {
			return 0, false
		}
		switch b[i] {
		case ',':
			i++
			continue
		case '}':
			return i + 1, true
		default:
			return 0, false
		}
	}
}

// skipArray consumes a JSON array: '[' (ws value (',' ws value)*)? ']'.
func skipArray(b []byte, i int) (int, bool) {
	i++ // '['
	i = skipWS(b, i)
	if i < len(b) && b[i] == ']' {
		return i + 1, true
	}
	for {
		i = skipWS(b, i)
		var ok bool
		if i, ok = skipValue(b, i); !ok {
			return 0, false
		}
		i = skipWS(b, i)
		if i >= len(b) {
			return 0, false
		}
		switch b[i] {
		case ',':
			i++
			continue
		case ']':
			return i + 1, true
		default:
			return 0, false
		}
	}
}
