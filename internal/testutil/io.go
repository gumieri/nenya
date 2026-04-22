package testutil

import (
	"bytes"
	"io"
	"time"
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

type delayReader struct {
	data   []byte
	offset int
	delay  time.Duration
}

func (r *delayReader) Read(p []byte) (int, error) {
	time.Sleep(r.delay)
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

type delayWriter struct {
	buf   *bytes.Buffer
	delay time.Duration
}

func (w *delayWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.buf.Write(p)
}

func (w *delayWriter) Close() error { return nil }

func (w *delayWriter) String() string {
	return w.buf.String()
}

func (w *delayWriter) Bytes() []byte {
	return w.buf.Bytes()
}

func (w *delayWriter) Reset() {
	w.buf.Reset()
}

type readCloser struct {
	reader io.Reader
	closed bool
}

func (rc *readCloser) Read(p []byte) (int, error) {
	return rc.reader.Read(p)
}

func (rc *readCloser) Close() error {
	rc.closed = true
	return nil
}

func (rc *readCloser) Closed() bool {
	return rc.closed
}

func NewDelayReader(data []byte, delay time.Duration) io.Reader {
	return &delayReader{data: data, delay: delay}
}

func NewDelayWriter(delay time.Duration) io.WriteCloser {
	return &delayWriter{buf: &bytes.Buffer{}, delay: delay}
}

func NewReadCloser(r io.Reader) io.ReadCloser {
	return &readCloser{reader: r}
}
