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
	runes := []rune(text)
	total := f.windowLen + len(runes)
	if total <= f.windowSize {
		f.window = append(f.window, runes...)
		f.windowLen = total
		return
	}
	if total > f.windowSize {
		drop := total - f.windowSize
		if drop >= f.windowLen {
			f.window = f.window[:0]
		} else {
			f.window = f.window[drop:]
		}
		f.window = append(f.window, runes...)
		f.windowLen = f.windowSize
	}
}

func (f *StreamEntropyFilter) WindowLen() int {
	return f.windowLen
}

func (f *StreamEntropyFilter) WindowContent() string {
	return string(f.window)
}
