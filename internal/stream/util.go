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
	*windowLen = maxSize
	return maxSize
}
