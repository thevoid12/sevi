package fetch

import (
	"strings"
	"testing"
)

func TestToSpeakable(t *testing.T) {
	md := "Title: x\n\nMarkdown Content:\n# Heading\n\nSee [the docs](https://x.com/a_(b)) and ![img](https://x.com/i.png).\n\n```go\ncode()\n```\n\n| a | b |\n\n- item **bold**[[4]]\n"
	got := ToSpeakable(md)
	for _, bad := range []string{"#", "http", "```", "|", "**", "[[4]]", "code()"} {
		if strings.Contains(got, bad) {
			t.Errorf("output still contains %q: %q", bad, got)
		}
	}
	for _, want := range []string{"Heading", "the docs", "item bold"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lost %q: %q", want, got)
		}
	}
}

func TestChunk(t *testing.T) {
	text := strings.Repeat("One sentence here. ", 50) + "\n\n" + strings.Repeat("x", 250)
	const n = 100
	chunks := Chunk(text, n)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if r := len([]rune(c)); r > n+1 { // +1: trailing space joined before trim
			t.Errorf("chunk %d has %d runes, limit %d", i, r, n)
		}
	}
	if joined := strings.Join(chunks, ""); !strings.Contains(joined, strings.Repeat("x", 250)[:100]) {
		t.Error("long unpunctuated run lost")
	}
}

func TestSlug(t *testing.T) {
	if got := Slug("https://example.com/a/b?q=1"); got != "example-com-a-b" {
		t.Errorf("got %q", got)
	}
}

func TestTitle(t *testing.T) {
	md := "Title: Cheap Satisfaction\n\nURL Source: https://x.com/a\n\nMarkdown Content:\nTitle: not this one\nBody"
	if got := Title(md); got != "Cheap Satisfaction" {
		t.Errorf("got %q", got)
	}
	if got := Title("no header here"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
