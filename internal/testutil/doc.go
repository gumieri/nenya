// Package testutil provides common utilities for testing Nenya components.
//
// It consolidates test infrastructure that was previously duplicated across
// multiple test files, including:
//
//   - Logger initialization (discard logger for silent tests)
//   - IO mock types (ErrorReader, ErrorWriter, BlockingReader, BytesCapture)
//   - Config and gateway factories
//   - HTTP request helpers
//
// All utilities are designed to be thread-safe and suitable for parallel tests.
package testutil
