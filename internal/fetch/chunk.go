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
