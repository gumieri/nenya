package util

import (
	"fmt"
	"math"
	"strings"
)

// AddCap returns a+b, clamped to math.MaxInt on overflow.
// Use this for slice capacity calculations where a+b could exceed
// the maximum int value.
func AddCap(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// JoinBackticks formats a slice of names as a comma-separated list
// wrapped in backticks. For example, ["foo", "bar"] becomes "`foo`, `bar`".
func JoinBackticks(names []string) string {
	var sb strings.Builder
	for i, name := range names {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('`')
		sb.WriteString(name)
		sb.WriteByte('`')
	}
	return sb.String()
}

// ErrNoProvider is the error message returned when no provider can be
// resolved for a given model name.
const ErrNoProvider = "No provider configured for this model"

// ErrNoProviderFmt returns ErrNoProvider formatted with the model name.
func ErrNoProviderFmt(model string) string {
	return fmt.Sprintf("%s: %s", ErrNoProvider, model)
}
