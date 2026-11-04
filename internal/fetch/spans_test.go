package fetch

import (
	"strings"
	"testing"
)

const sample = "First paragraph here. It has two sentences.\n\nSecond paragraph is short.\nThird line, no end\n\nFourth one is a bit longer than the others, on purpose. And it goes on. And on."

func TestSpansTileTheText(t *testing.T) {
	r := []rune(sample)
	for _, n := range []int{20, 40, 60, 100, 1000} {
		spans := Spans(sample, n)
		if len(spans) == 0 {
			t.Fatalf("n=%d: no spans", n)
		}
		cursor := 0
		for _, sp := range spans {
			if sp.Start != cursor || sp.End <= sp.Start {
				t.Fatalf("n=%d: spans do not tile: %+v at cursor %d", n, sp, cursor)
			}
			if sp.Text != strings.TrimSpace(string(r[sp.Start:sp.End])) || sp.Text == "" {
				t.Errorf("n=%d: span text does not match its range: %+v", n, sp)
			}
			cursor = sp.End
		}
		if cursor != len(r) {
			t.Errorf("n=%d: spans end at %d, text has %d runes", n, cursor, len(r))
		}
	}
}

func TestSpansRespectTheSizeAndSentenceEnds(t *testing.T) {
	for _, sp := range Spans(sample, 60) {
		if len([]rune(sp.Text)) > 60 {
			t.Errorf("chunk too long (%d): %q", len([]rune(sp.Text)), sp.Text)
		}
	}
	// A chunk boundary is after a sentence end whenever the text allows it.
	spans := Spans(sample, 60)
	for _, sp := range spans[:len(spans)-1] {
		if !strings.HasSuffix(sp.Text, ".") && !strings.HasSuffix(sp.Text, "end") {
			t.Errorf("chunk does not end at a sentence: %q", sp.Text)
		}
	}
}

func TestSpansInOnlyCoversTheGap(t *testing.T) {
	all := Spans(sample, 60)
	gapFrom, gapTo := all[1].Start, all[2].End
	gap := SpansIn(sample, gapFrom, gapTo, 25)
	cursor := gapFrom
	for _, sp := range gap {
		if sp.Start != cursor {
			t.Fatalf("gap spans do not tile: %+v at %d", sp, cursor)
		}
		cursor = sp.End
	}
	if cursor != gapTo {
		t.Errorf("gap spans end at %d, want %d", cursor, gapTo)
	}
	// Different chunk size on the gap, same text range covered.
	var got strings.Builder
	for _, sp := range gap {
		got.WriteString(sp.Text + " ")
	}
	want := strings.Join(strings.Fields(string([]rune(sample)[gapFrom:gapTo])), " ")
	if strings.Join(strings.Fields(got.String()), " ") != want {
		t.Errorf("gap text differs:\n got %q\nwant %q", strings.Join(strings.Fields(got.String()), " "), want)
	}
}

func TestSpansOfAnUnbrokenRun(t *testing.T) {
	run := strings.Repeat("a", 250)
	spans := Spans(run, 100)
	if len(spans) != 3 || spans[2].End != 250 {
		t.Errorf("spans: %+v", spans)
	}
}
