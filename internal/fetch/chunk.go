package fetch

import (
	"regexp"
	"strings"
)

var reSentence = regexp.MustCompile(`[^.!?。！？]+[.!?。！？]*\s*`)

// Chunk splits on paragraph/sentence boundaries into pieces of at most n runes.
func Chunk(s string, n int) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for _, p := range strings.Split(s, "\n") {
		for _, sent := range splitSentences(p, n) {
			if cur.Len() > 0 && len([]rune(cur.String()))+len([]rune(sent)) > n {
				flush()
			}
			cur.WriteString(sent + " ")
		}
		cur.WriteString("\n")
	}
	flush()
	return out
}

func splitSentences(p string, n int) []string {
	var out []string
	for _, s := range reSentence.FindAllString(p, -1) {
		r := []rune(s)
		for len(r) > n { // pathological no-punctuation run
			out = append(out, string(r[:n]))
			r = r[n:]
		}
		out = append(out, string(r))
	}
	return out
}

// SplitHalf cuts s into two parts of about equal length at the sentence end
// nearest the middle (a space if there is no sentence end). ok is false when s is
// too short to cut.
func SplitHalf(s string) (a, b string, ok bool) {
	r := []rune(s)
	if len(r) < 2 {
		return "", "", false
	}
	mid := len(r) / 2
	best, bestDist := -1, len(r)
	consider := func(i int) { // cut after rune i-1
		if i <= 0 || i >= len(r) {
			return
		}
		if d := abs(i - mid); d < bestDist {
			best, bestDist = i, d
		}
	}
	for i := range r {
		switch r[i] {
		case '.', '!', '?', '。', '！', '？', '\n':
			consider(i + 1)
		}
	}
	if best < 0 { // no sentence end: cut at a space
		for i := range r {
			if r[i] == ' ' {
				consider(i + 1)
			}
		}
	}
	if best < 0 {
		best = mid // one unbroken run
	}
	a, b = strings.TrimSpace(string(r[:best])), strings.TrimSpace(string(r[best:]))
	if a == "" || b == "" {
		return "", "", false
	}
	return a, b, true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
