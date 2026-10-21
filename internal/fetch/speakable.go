package fetch

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	// link/image targets may contain one level of parens, e.g. /wiki/Go_(lenguaje)
	reImage  = regexp.MustCompile(`!\[[^\]]*\]\((?:[^()]|\([^)]*\))*\)`)
	reLink   = regexp.MustCompile(`\[([^\]]*)\]\((?:[^()]|\([^)]*\))*\)`)
	reFoot   = regexp.MustCompile(`\[\[?[\w ]{1,12}\]?\]`) // [[4]], [editar]
	reCode   = regexp.MustCompile("(?s)```.*?```")
	reURL    = regexp.MustCompile(`https?://\S+`)
	reTable  = regexp.MustCompile(`(?m)^\s*\|.*$`)
	reMarks  = regexp.MustCompile("(?m)^\\s{0,3}(#{1,6}|>|[-*+]|\\d+\\.)\\s+|[*_`~]{1,3}")
	reBlanks = regexp.MustCompile(`\n{3,}`)
)

// ToSpeakable strips markdown noise so TTS doesn't read symbols aloud.
func ToSpeakable(md string) string {
	if i := strings.Index(md, "Markdown Content:"); i >= 0 {
		md = md[i+len("Markdown Content:"):]
	}
	md = reCode.ReplaceAllString(md, "")
	md = reTable.ReplaceAllString(md, "")
	md = reImage.ReplaceAllString(md, "")
	md = reLink.ReplaceAllString(md, "$1")
	md = reFoot.ReplaceAllString(md, "")
	md = reURL.ReplaceAllString(md, "")
	md = reMarks.ReplaceAllString(md, "")
	return strings.TrimSpace(reBlanks.ReplaceAllString(md, "\n\n"))
}

// DetectScript reports the dominant writing system (for logging).
func DetectScript(s string) string {
	scripts := map[string]*unicode.RangeTable{
		"Tamil": unicode.Tamil, "Devanagari": unicode.Devanagari, "Bengali": unicode.Bengali,
		"Telugu": unicode.Telugu, "Arabic": unicode.Arabic, "Cyrillic": unicode.Cyrillic,
		"Han": unicode.Han, "Hiragana": unicode.Hiragana, "Hangul": unicode.Hangul,
		"Greek": unicode.Greek, "Latin": unicode.Latin,
	}
	counts := map[string]int{}
	for _, r := range s {
		for name, t := range scripts {
			if unicode.Is(t, r) {
				counts[name]++
			}
		}
	}
	best, n := "unknown", 0
	for name, c := range counts {
		if c > n {
			best, n = name, c
		}
	}
	return best
}

var reTitle = regexp.MustCompile(`(?m)^Title:[ \t]*(.+?)[ \t]*$`)

// Title returns the page title from Jina's header ("Title: ..."), or "".
func Title(md string) string {
	head := md
	if i := strings.Index(md, "Markdown Content:"); i >= 0 {
		head = md[:i]
	}
	if m := reTitle.FindStringSubmatch(head); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}
