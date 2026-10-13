// sevi: URL -> article text (Jina Reader) -> Gemini TTS via OpenRouter -> output/<slug>.mp3
// Usage: go run . <url>
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	ttsModel   = "google/gemini-3.8-flash-tts"
	ttsURL     = "https://openrouter.ai/api/v1/audio/speech"
	voice      = "Kore"
	chunkChars = 3000 // well under the 8192 input-token limit
)

func main() {
	if len(os.Args) < 2 {
		fatal("usage: go run . <url>")
	}
	loadEnv(".env")
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		fatal("OPENROUTER_API_KEY missing in .env")
	}

	md, err := fetchMarkdown(os.Args[1])
	check(err)
	text := toSpeakable(md)
	fmt.Printf("extracted %d chars, dominant script: %s\n", len([]rune(text)), detectScript(text))

	// Gemini TTS auto-detects the language from the text itself.
	var pcm []byte
	var genIDs []string
	chunks := chunk(text, chunkChars)
	for i, c := range chunks {
		fmt.Printf("tts chunk %d/%d\n", i+1, len(chunks))
		b, id, err := synth(key, c)
		if err != nil {
			fmt.Fprintln(os.Stderr, "skipping chunk:", err)
			continue
		}
		pcm = append(pcm, b...)
		genIDs = append(genIDs, id)
	}

	check(os.MkdirAll("output", 0o755))
	out := filepath.Join("output", slug(os.Args[1])+".mp3")
	check(encodeMP3(pcm, out))
	fmt.Println("wrote", out)

	costPath := strings.TrimSuffix(out, ".mp3") + ".cost.md"
	check(writeCost(costPath, key, os.Args[1], len(chunks), genIDs))
	fmt.Println("wrote", costPath)
}

// fetchMarkdown uses Jina Reader: GET r.jina.ai/<url> returns the page as markdown.
func fetchMarkdown(u string) (string, error) {
	req, _ := http.NewRequest("GET", "https://r.jina.ai/"+u, nil)
	req.Header.Set("Accept", "text/plain")
	if k := os.Getenv("JINA_API_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	body, _, err := do(req)
	return string(body), err
}

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

// toSpeakable strips markdown noise so TTS doesn't read symbols aloud.
func toSpeakable(md string) string {
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

// detectScript reports the dominant writing system (log only).
func detectScript(s string) string {
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

// chunk splits on paragraph/sentence boundaries into pieces of at most n runes.
func chunk(s string, n int) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for _, p := range strings.Split(s, "\n") {
		for _, sent := range splitSentences(p) {
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

var reSentence = regexp.MustCompile(`[^.!?。！？]+[.!?。！？]*\s*`)

func splitSentences(p string) []string {
	var out []string
	for _, s := range reSentence.FindAllString(p, -1) {
		r := []rune(s)
		for len(r) > chunkChars { // pathological no-punctuation run
			out = append(out, string(r[:chunkChars]))
			r = r[chunkChars:]
		}
		out = append(out, string(r))
	}
	return out
}

func synth(key, text string) ([]byte, string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"model":           ttsModel,
		"input":           text,
		"voice":           voice,
		"response_format": "pcm", // Gemini TTS supports pcm only (24kHz mono s16le)
	})
	req, _ := http.NewRequest("POST", ttsURL, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	return retry(3, req, reqBody) // audio bytes + OpenRouter generation id
}

// retry re-sends on 5xx/empty-audio flakes from the upstream provider.
func retry(n int, req *http.Request, body []byte) ([]byte, string, error) {
	var err error
	for i := 0; i < n; i++ {
		req.Body = io.NopCloser(bytes.NewReader(body))
		var b []byte
		var h http.Header
		if b, h, err = do(req); err == nil && len(b) > 0 {
			return b, h.Get("X-Generation-Id"), nil
		}
		time.Sleep(time.Duration(i+1) * 2 * time.Second)
	}
	return nil, "", fmt.Errorf("after %d tries: %v", n, err)
}

// encodeMP3 pipes 24kHz mono s16le PCM through ffmpeg.
func encodeMP3(pcm []byte, out string) error {
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "s16le", "-ar", "24000", "-ac", "1", "-i", "-", out)
	cmd.Stdin = bytes.NewReader(pcm)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func do(req *http.Request) ([]byte, http.Header, error) {
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("%s: %s: %.300s", req.URL.Host, resp.Status, b)
	}
	return b, resp.Header, nil
}

func slug(u string) string {
	p, _ := url.Parse(u)
	s := strings.Trim(regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(p.Host+p.Path, "-"), "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && !strings.HasPrefix(k, "#") {
			if os.Getenv(k) == "" {
				os.Setenv(k, strings.Trim(v, `"'`))
			}
		}
	}
}

func check(err error) {
	if err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

// writeCost looks up each TTS generation on OpenRouter and writes a markdown cost report.
func writeCost(path, key, src string, nChunks int, ids []string) error {
	var total float64
	var in, outTok int
	var rows strings.Builder
	for i, id := range ids {
		g, err := generation(key, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cost lookup failed:", id, err)
			fmt.Fprintf(&rows, "| %d | %s | ? | ? | ? |\n", i+1, id)
			continue
		}
		total += g.Usage
		in += g.NativeTokensPrompt
		outTok += g.NativeTokensCompletion
		fmt.Fprintf(&rows, "| %d | %s | %d | %d | $%.6f |\n", i+1, id, g.NativeTokensPrompt, g.NativeTokensCompletion, g.Usage)
	}
	md := fmt.Sprintf(`# Cost: %s

- Source: %s
- Model: %s (via OpenRouter)
- Chunks: %d (%d synthesized)
- Input tokens: %d
- Output (audio) tokens: %d
- **Total: $%.6f**

| # | generation id | in tokens | out tokens | cost |
|---|---|---|---|---|
%s`, filepath.Base(path), src, ttsModel, nChunks, len(ids), in, outTok, total, rows.String())
	return os.WriteFile(path, []byte(md), 0o644)
}

type generationInfo struct {
	Usage                  float64 `json:"usage"`
	NativeTokensPrompt     int     `json:"native_tokens_prompt"`
	NativeTokensCompletion int     `json:"native_tokens_completion"`
}

// generation polls /generation until the record appears (it lags the request by a few seconds).
func generation(key, id string) (generationInfo, error) {
	var err error
	for i := 0; i < 10; i++ {
		time.Sleep(3 * time.Second)
		req, _ := http.NewRequest("GET", "https://openrouter.ai/api/v1/generation?id="+id, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		var b []byte
		if b, _, err = do(req); err != nil {
			continue
		}
		var r struct {
			Data generationInfo `json:"data"`
		}
		if err = json.Unmarshal(b, &r); err == nil {
			return r.Data, nil
		}
	}
	return generationInfo{}, err
}
