package fetch

import (
	"strings"
	"testing"
)

func TestSplitHalfCutsAtTheSentenceNearestTheMiddle(t *testing.T) {
	s := "One is short. Two is a little bit longer than one. Three. Four is the last one here."
	a, b, ok := SplitHalf(s)
	if !ok || !strings.HasSuffix(a, ".") || strings.Join([]string{a, b}, " ") != s {
		t.Fatalf("got %q | %q ok=%v", a, b, ok)
	}
	if d := len([]rune(a)) - len([]rune(b)); d > 20 || d < -20 {
		t.Errorf("halves are lopsided: %d vs %d", len([]rune(a)), len([]rune(b)))
	}
}

func TestSplitHalfFallsBackToSpacesAndTinyInputs(t *testing.T) {
	a, b, ok := SplitHalf("no punctuation in this run of words at all")
	if !ok || a == "" || b == "" || strings.Contains(a, "  ") {
		t.Errorf("space split: %q | %q ok=%v", a, b, ok)
	}
	if _, _, ok := SplitHalf("x"); ok {
		t.Error("one rune cannot be split")
	}
	a, b, ok = SplitHalf(strings.Repeat("z", 100))
	if !ok || len(a) != 50 || len(b) != 50 {
		t.Errorf("unbroken run: %d %d %v", len(a), len(b), ok)
	}
}
