package stream

type RedactFunc func(text string, label string) string

type StreamEntropyFilter struct {
	redact      RedactFunc
	redactLabel string
	window      []rune
	windowSize  int
	windowLen   int
}

func NewStreamEntropyFilter(redact RedactFunc, redactLabel string, windowSize int) *StreamEntropyFilter {
	if windowSize <= 0 {
		windowSize = 4096
	}
	return &StreamEntropyFilter{
		redact:      redact,
		redactLabel: redactLabel,
		window:      make([]rune, 0, windowSize),
		windowSize:  windowSize,
	}
}

func (f *StreamEntropyFilter) FilterContent(content string) (string, FilterAction) {
	if len(content) == 0 {
		return content, ActionPass
	}

	f.appendToWindow(content)

	redacted := f.redact(content, f.redactLabel)
	if redacted != content {
		return redacted, ActionRedact
	}

	return content, ActionPass
}

func (f *StreamEntropyFilter) appendToWindow(text string) {
	f.windowLen = AppendRuneWindow(&f.window, &f.windowLen, f.windowSize, text)
}

func (f *StreamEntropyFilter) WindowLen() int {
	return f.windowLen
}

func (f *StreamEntropyFilter) WindowContent() string {
	return string(f.window)
}
