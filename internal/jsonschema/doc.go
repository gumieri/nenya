// Package jsonschema normalizes tool JSON schemas for cross-provider
// transport (NENYA-172). It is a stdlib-only leaf: it must not import any other
// repository package.
//
// Providers reject a surprising amount of legal JSON Schema with a 400: dialect
// and meta keywords ($schema, $id, $anchor, $comment, ...), bare boolean
// subschemas, unresolved $ref pointers, regexes in a flavor the provider does
// not implement (Unicode property escapes), NUL and other C0 control characters
// in strings, and schemas whose root declares no type. The upstream evidence is
// a month of schema-sanitization fixes in CLIProxyAPI (dialect-keyword
// stripping, Unicode property escapes plus a fast-path bypass in
// patternProperties inspection, identifier keywords, boolean subschemas, type
// inference with uppercase tolerance, constraint preservation, octal NUL escape
// stripping, $ref stringification, tool-name namespacing, parameterless-tool
// fallback).
//
// Sanitize is total and non-failing: every input produces a valid object schema,
// so a hostile or broken tool schema can never turn into an upstream 400 or a
// dropped request. It walks EVERY subschema position (there is deliberately no
// fast path), resolves local $ref pointers with a cycle cap, and preserves the
// constraints it does not understand (dropping an unknown constraint is what
// makes a provider reject the schema).
package jsonschema
