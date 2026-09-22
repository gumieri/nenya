package stream

// AppendRuneWindow appends text to a sliding window of runes,
// evicting oldest content when window exceeds maxSize.
// Returns the new window length.
func AppendRuneWindow(window *[]rune, windowLen *int, maxSize int, text string) int {
	runes := []rune(text)
	total := *windowLen + len(runes)
	if total <= maxSize {
		*window = append(*window, runes...)
		*windowLen = total
		return total
	}
	drop := total - maxSize
	if drop >= *windowLen {
		*window = (*window)[:0]
	} else {
		*window = (*window)[drop:]
	}
	*window = append(*window, runes...)
	// An oversized single chunk can push the slice past maxSize (the
	// append above only evicts prior content); clamp so the slice and
	// the length counter stay in sync — otherwise the window grows
	// without bound on streams of oversized deltas.
	if len(*window) > maxSize {
		*window = (*window)[len(*window)-maxSize:]
	}
	*windowLen = len(*window)
	return *windowLen
}
