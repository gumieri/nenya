package testutil

import (
	"bytes"
	"net/http"
)

// BrokenWriter is an http.ResponseWriter that always returns a fixed error on Write.
// Useful for testing error handling in HTTP handlers.
type BrokenWriter struct {
	Err      error
	headers  http.Header
	Flushed  bool
	Written  bool
	buf      *bytes.Buffer
}

// NewBrokenWriter creates a new BrokenWriter with the given error.
func NewBrokenWriter(err error) *BrokenWriter {
	return &BrokenWriter{
		Err:     err,
		headers: make(http.Header),
		buf:     &bytes.Buffer{},
	}
}

func (w *BrokenWriter) Write(p []byte) (n int, err error) {
	w.Written = true
	if w.Err != nil {
		return 0, w.Err
	}
	return w.buf.Write(p)
}

func (w *BrokenWriter) WriteHeader(statusCode int) {
	// No-op for testing
}

func (w *BrokenWriter) Header() http.Header {
	return w.headers
}

func (w *BrokenWriter) Flush() {
	w.Flushed = true
}

// Bytes returns the bytes written to this writer (if no error occurred).
func (w *BrokenWriter) Bytes() []byte {
	return w.buf.Bytes()
}

// String returns the string written to this writer (if no error occurred).
func (w *BrokenWriter) String() string {
	return w.buf.String()
}

// Reset clears the buffer and state.
func (w *BrokenWriter) Reset() {
	w.buf.Reset()
	w.Flushed = false
	w.Written = false
}
