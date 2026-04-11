package pipeline

import (
	"regexp"
	"strings"
)

type CodeSpan struct {
	Start    int
	End      int
	Language string
}

var fenceRe = regexp.MustCompile("(?m)^(`{3,})(\\w*)\\s*$")

func DetectCodeFences(text string) []CodeSpan {
	matches := fenceRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches)%2 != 0 {
		return nil
	}

	var spans []CodeSpan
	for i := 0; i < len(matches); i += 2 {
		openMatch := matches[i]
		closeMatch := matches[i+1]

		if closeMatch[0] <= openMatch[0] {
			continue
		}

		lang := ""
		if openMatch[4] != -1 && openMatch[5] != -1 {
			lang = strings.ToLower(text[openMatch[4]:openMatch[5]])
		}

		spans = append(spans, CodeSpan{
			Start:    openMatch[0],
			End:      closeMatch[1],
			Language: lang,
		})
	}

	return spans
}

func HasCodeFences(text string) bool {
	return len(DetectCodeFences(text)) > 0
}
