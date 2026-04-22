package testutil

import (
	"bytes"
	"io"
)

// ErrorReader is an io.Reader that always returns a fixed error.
type ErrorReader struct {
	Err error
}

func (e *ErrorReader) Read(p []byte) (n int, err error) {
	return 0, e.Err
}

// ErrorWriter is an io.Writer that always returns a fixed error.
type ErrorWriter struct {
	Err error
}

func (e *ErrorWriter) Write(p []byte) (n int, err error) {
	return 0, e.Err
}

// BlockingReader is an io.Reader that blocks until Close is called.
// Useful for testing timeouts and cancellation.
type BlockingReader struct {
	Closed chan struct{}
}

func (b *BlockingReader) Read(p []byte) (n int, err error) {
	<-b.Closed
	return 0, io.EOF
}

func (b *BlockingReader) Close() error {
	close(b.Closed)
	return nil
}

// BytesCapture is an io.Writer that captures all writes to a buffer.
type BytesCapture struct {
	buf *bytes.Buffer
}

func NewBytesCapture() *BytesCapture {
	return &BytesCapture{buf: &bytes.Buffer{}}
}

func (b *BytesCapture) Write(p []byte) (n int, err error) {
	return b.buf.Write(p)
}

func (b *BytesCapture) Bytes() []byte {
	return b.buf.Bytes()
}

func (b *BytesCapture) String() string {
	return b.buf.String()
}

func (b *BytesCapture) Reset() {
	b.buf.Reset()
}
