//go:generate curl -sL https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken -o cl100k_base.tiktoken
//go:generate echo "Expected SHA-256: 223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7"
//go:generate sha256sum cl100k_base.tiktoken

package tiktoken

import (
	"bufio"
	_ "embed"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"math"
	"strings"
	"sync"
	"unicode"
)

//go:embed cl100k_base.tiktoken
var embeddedVocab []byte

const expectedChecksum = "223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7"

var (
	ranks    map[string]uint32
	initOnce sync.Once
)

func init() {
	initOnce.Do(loadVocab)
}

func loadVocab() {
	checksum := sha256.Sum256(embeddedVocab)
	if hex.EncodeToString(checksum[:]) != expectedChecksum {
		panic("tiktoken: embedded vocab checksum mismatch — file may be corrupted or tampered with")
	}

	ranks = make(map[string]uint32, 100256)
	br := bufio.NewReader(strings.NewReader(string(embeddedVocab)))
	for {
		line, err := br.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			panic("tiktoken: failed to parse vocab: " + err.Error())
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		tokenBytes, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			panic("tiktoken: failed to base64-decode token: " + err.Error())
		}
		var rank uint32
		if _, err := parseUint32(parts[1], &rank); err != nil {
			panic("tiktoken: failed to parse rank: " + err.Error())
		}
		ranks[string(tokenBytes)] = rank
	}
}

func parseUint32(s string, out *uint32) (bool, error) {
	var val uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false, nil
		}
		val = val*10 + uint64(c-'0')
		if val > math.MaxUint32 {
			return false, nil
		}
	}
	*out = uint32(val)
	return true, nil
}

func CountTokens(text string) int {
	if text == "" {
		return 0
	}
	pieces := preTokenize(text)
	total := 0
	for _, piece := range pieces {
		total += bpeCount([]byte(piece))
	}
	return total
}

func preTokenize(text string) []string {
	var pieces []string
	runes := []rune(text)
	n := len(runes)
	i := 0
	for i < n {
		end := tryContraction(runes, i, n)
		if end > i {
			pieces = append(pieces, string(runes[i:end]))
			i = end
			continue
		}

		end = tryLetters(runes, i, n)
		if end > i {
			pieces = append(pieces, string(runes[i:end]))
			i = end
			continue
		}

		end = tryDigits(runes, i, n)
		if end > i {
			pieces = append(pieces, string(runes[i:end]))
			i = end
			continue
		}

		end = tryPunctuation(runes, i, n)
		if end > i {
			pieces = append(pieces, string(runes[i:end]))
			i = end
			continue
		}

		end = tryTrailingWhitespace(runes, i, n)
		if end > i {
			pieces = append(pieces, string(runes[i:end]))
			i = end
			continue
		}

		end = tryNewlineChunk(runes, i, n)
		if end > i {
			pieces = append(pieces, string(runes[i:end]))
			i = end
			continue
		}

		end = tryWhitespaceNotBeforeContent(runes, i, n)
		if end > i {
			pieces = append(pieces, string(runes[i:end]))
			i = end
			continue
		}

		end = trySingleWhitespace(runes, i, n)
		if end > i {
			pieces = append(pieces, string(runes[i:end]))
			i = end
			continue
		}

		pieces = append(pieces, string(runes[i]))
		i++
	}
	return pieces
}

func tryContraction(runes []rune, i, n int) int {
	if runes[i] != '\'' || i+1 >= n {
		return i
	}
	next := unicode.ToLower(runes[i+1])
	switch next {
	case 's', 't', 'm', 'd':
		return i + 2
	case 'l':
		if i+2 < n && unicode.ToLower(runes[i+2]) == 'l' {
			return i + 3
		}
	case 'v':
		if i+2 < n && unicode.ToLower(runes[i+2]) == 'e' {
			return i + 3
		}
	case 'r':
		if i+2 < n && unicode.ToLower(runes[i+2]) == 'e' {
			return i + 3
		}
	}
	return i
}

func tryLetters(runes []rune, i, n int) int {
	if !unicode.IsLetter(runes[i]) {
		return tryLettersWithPrefix(runes, i, n)
	}
	for i < n && unicode.IsLetter(runes[i]) {
		i++
	}
	return i
}

func tryLettersWithPrefix(runes []rune, i, n int) int {
	if i >= n {
		return i
	}
	r := runes[i]
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\r' || r == '\n' {
		return i
	}
	prefix := i + 1
	if prefix >= n || !unicode.IsLetter(runes[prefix]) {
		return i
	}
	for j := prefix; j < n && unicode.IsLetter(runes[j]); j++ {
		i = j + 1
	}
	return i
}

func tryDigits(runes []rune, i, n int) int {
	if !unicode.IsDigit(runes[i]) {
		return i
	}
	count := 0
	for i < n && unicode.IsDigit(runes[i]) && count < 3 {
		i++
		count++
	}
	return i
}

func tryPunctuation(runes []rune, i, n int) int {
	r := runes[i]
	if !(unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsMark(r)) {
		return i
	}
	if i+1 < n && runes[i] == ' ' {
		return i
	}
	for i < n && (unicode.IsPunct(runes[i]) || unicode.IsSymbol(runes[i]) || unicode.IsMark(runes[i])) {
		i++
	}
	for i < n && (runes[i] == '\r' || runes[i] == '\n') {
		i++
	}
	return i
}

func tryTrailingWhitespace(runes []rune, i, n int) int {
	if i == n || !isSpace(runes[i]) {
		return i
	}
	orig := i
	for i < n && isSpace(runes[i]) {
		i++
	}
	if i != n {
		return orig
	}
	return i
}

func tryNewlineChunk(runes []rune, i, n int) int {
	if i >= n {
		return i
	}
	orig := i
	for i < n && isSpace(runes[i]) && runes[i] != '\r' && runes[i] != '\n' {
		i++
	}
	if i >= n || (runes[i] != '\r' && runes[i] != '\n') {
		return orig
	}
	for i < n && (runes[i] == '\r' || runes[i] == '\n') {
		i++
	}
	return i
}

func tryWhitespaceNotBeforeContent(runes []rune, i, n int) int {
	if i >= n || !isSpace(runes[i]) {
		return i
	}
	orig := i
	for i < n && isSpace(runes[i]) {
		i++
	}
	if i < n && !isSpace(runes[i]) {
		i--
	}
	if i <= orig {
		return orig
	}
	return i
}

func trySingleWhitespace(runes []rune, i, n int) int {
	if i < n && isSpace(runes[i]) {
		return i + 1
	}
	return i
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func bpeCount(piece []byte) int {
	n := len(piece)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return 1
	}
	if _, ok := ranks[string(piece)]; ok {
		return 1
	}

	parts := make([]int, n+1)
	for i := range parts {
		parts[i] = i
	}

	partsRank := make([]int, n)
	for i := 0; i < n-1; i++ {
		partsRank[i] = getRank(piece, parts[i], parts[i+2])
	}
	partsRank[n-1] = math.MaxInt

	for {
		minRank := math.MaxInt
		minIdx := -1
		for i := 0; i < n-1; i++ {
			if partsRank[i] < minRank {
				minRank = partsRank[i]
				minIdx = i
			}
		}
		if minRank == math.MaxInt {
			break
		}

		parts[minIdx+1] = parts[minIdx]
		copy(parts[minIdx:], parts[minIdx+1:])
		n--
		if n == 1 {
			break
		}
		if minIdx > 0 {
			partsRank[minIdx-1] = getRank(piece, parts[minIdx-1], parts[minIdx+1])
		}
		if minIdx < n-1 {
			partsRank[minIdx] = getRank(piece, parts[minIdx], parts[minIdx+2])
		}
		copy(partsRank[minIdx+1:n], partsRank[minIdx+2:n+1])
		partsRank[n-1] = math.MaxInt
	}
	return n
}

func getRank(piece []byte, start, end int) int {
	if start < 0 || end > len(piece) || start >= end {
		return math.MaxInt
	}
	if rank, ok := ranks[string(piece[start:end])]; ok {
		return int(rank)
	}
	return math.MaxInt
}
