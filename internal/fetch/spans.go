package fetch

import (
	"strings"
	"unicode/utf8"

	"sevi/internal/model"
)

// Spans splits the whole text like Chunk, keeping positions.
func Spans(text string, n int) []model.Span {
	return SpansIn(text, 0, len([]rune(text)), n)
}

// SpansIn splits only the runes [from, to) of text into chunks of at most n
// runes, cutting at paragraph and sentence ends. The returned offsets are
// relative to the whole text and tile [from, to). Chunks with nothing to say
// (whitespace only) are folded into their neighbour.
func SpansIn(text string, from, to, n int) []model.Span {
	r := []rune(text)
	from, to = max(from, 0), min(to, len(r))
	if from >= to || n <= 0 {
		return nil
	}
	var units [][2]int // sentence (or piece of a long run) as absolute rune offsets
	off := from
	for _, p := range strings.Split(string(r[from:to]), "\n") {
		pr := []rune(p)
		bytePos, runePos := 0, 0 // advanced as the matches go by, so the paragraph is read once
		for _, s := range reSentence.FindAllStringIndex(p, -1) {
			runePos += utf8.RuneCountInString(p[bytePos:s[0]])
			a := runePos
			b := a + utf8.RuneCountInString(p[s[0]:s[1]])
			bytePos, runePos = s[1], b
			for b-a > n { // a sentence longer than a chunk: cut at the last space that fits, else hard
				cut := n
				for i := n - 1; i > 0; i-- {
					if r[off+a+i] == ' ' {
						cut = i + 1
						break
					}
				}
				units = append(units, [2]int{off + a, off + a + cut})
				a += cut
			}
			units = append(units, [2]int{off + a, off + b})
		}
		off += len(pr) + 1 // the newline
	}
	if len(units) == 0 {
		return nil
	}
	// Group units into chunks of at most n runes of the original text.
	var starts []int
	chunkStart := units[0][0]
	for _, u := range units {
		if u[1]-chunkStart > n && u[0] > chunkStart {
			starts = append(starts, chunkStart)
			chunkStart = u[0]
		}
	}
	starts = append(starts, chunkStart)

	var out []model.Span
	for i, s := range starts {
		start, end := s, to
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		if i == 0 {
			start = from // anything before the first sentence belongs to the first chunk
		}
		text := strings.TrimSpace(string(r[start:end]))
		if text == "" {
			if len(out) > 0 {
				out[len(out)-1].End = end
			}
			continue
		}
		out = append(out, model.Span{Start: start, End: end, Text: text})
	}
	return out
}
