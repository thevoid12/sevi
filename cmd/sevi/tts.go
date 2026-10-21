package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"

	"sevi/internal/audio"
	"sevi/internal/config"
	"sevi/internal/fetch"
	"sevi/internal/provider"
)

// runTTS is the v0 flow: URL -> clean text -> TTS per chunk -> one MP3 + cost report.
// If a chunk fails after retries the job stops with an error naming the chunk,
// and the audio of the chunks that did succeed is kept as <slug>.partial.mp3.
func runTTS(ctx context.Context, log *zap.SugaredLogger, cfg *config.Config, pageURL string) error {
	ttsCfg := cfg.Models.TTS
	prov := cfg.Provider(ttsCfg.Provider)
	key, err := cfg.ProviderKey(ttsCfg.Provider)
	if err != nil {
		return err
	}

	var jinaURL, jinaKey = "https://r.jina.ai", ""
	if j, ok := cfg.Providers["jina"]; ok {
		jinaURL, jinaKey = j.BaseURL, os.Getenv(j.KeyEnv)
	}
	md, err := fetch.Markdown(ctx, jinaURL, jinaKey, pageURL)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", pageURL, err)
	}
	text := fetch.ToSpeakable(md)
	chunks := fetch.Chunk(text, ttsCfg.ChunkChars)
	log.Infow("extracted", "url", pageURL, "chars", len([]rune(text)), "script", fetch.DetectScript(text), "chunks", len(chunks))
	if len(chunks) == 0 {
		return fmt.Errorf("no speakable text found at %s", pageURL)
	}

	tts := &provider.OpenRouterTTS{
		BaseURL: prov.BaseURL, Key: key, Model: ttsCfg.Model, Voice: ttsCfg.Voice,
		Retries: cfg.App.Retries, Log: log,
	}

	if err := os.MkdirAll(cfg.App.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	base := filepath.Join(cfg.App.OutputDir, fetch.Slug(pageURL))

	var pcm [][]byte
	var genIDs []string
	for i, c := range chunks {
		log.Infow("tts chunk", "n", i+1, "of", len(chunks), "chars", len([]rune(c)))
		sp, err := tts.Synth(ctx, c)
		if err != nil {
			if len(pcm) > 0 {
				partial := base + ".partial.mp3"
				if perr := audio.EncodeMP3(ctx, audio.Stitch(pcm), partial); perr != nil {
					log.Errorw("could not save partial audio", "err", perr)
				} else {
					log.Infow("partial audio kept", "path", partial, "chunks_done", len(pcm))
				}
			}
			return fmt.Errorf("chunk %d of %d failed (model %s): %w", i+1, len(chunks), ttsCfg.Model, err)
		}
		pcm = append(pcm, sp.PCM)
		genIDs = append(genIDs, sp.GenerationID)
	}

	all := audio.Stitch(pcm)
	out := base + ".mp3"
	if err := audio.EncodeMP3(ctx, all, out); err != nil {
		return err
	}
	log.Infow("wrote audio", "path", out, "seconds", int(audio.Duration(all)))
	fmt.Println("wrote", out)

	costPath := base + ".cost.md"
	if err := writeCostReport(ctx, log, cfg, key, costPath, pageURL, len(chunks), genIDs); err != nil {
		return err
	}
	fmt.Println("wrote", costPath)
	return nil
}

func writeCostReport(ctx context.Context, log *zap.SugaredLogger, cfg *config.Config, key, path, src string, nChunks int, ids []string) error {
	prov := cfg.Provider(cfg.Models.TTS.Provider)

	type row struct {
		u   provider.Usage
		err error
	}
	rows := make([]row, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows[i].u, rows[i].err = provider.OpenRouterUsage(ctx, prov.BaseURL, key, id)
		}()
	}
	wg.Wait()

	var total float64
	var in, out int
	var tbl strings.Builder
	for i, r := range rows {
		if r.err != nil {
			log.Errorw("cost lookup failed", "generation", ids[i], "err", r.err)
			fmt.Fprintf(&tbl, "| %d | %s | ? | ? | ? |\n", i+1, ids[i])
			continue
		}
		total += r.u.CostUSD
		in += r.u.PromptTokens
		out += r.u.CompletionTokens
		fmt.Fprintf(&tbl, "| %d | %s | %d | %d | $%.6f |\n", i+1, ids[i], r.u.PromptTokens, r.u.CompletionTokens, r.u.CostUSD)
	}
	log.Infow("cost", "total_usd", total, "input_tokens", in, "output_tokens", out, "model", cfg.Models.TTS.Model)

	md := fmt.Sprintf(`# Cost: %s

- Source: %s
- Model: %s (via %s)
- Chunks: %d
- Input tokens: %d
- Output (audio) tokens: %d
- **Total: $%.6f**

| # | generation id | in tokens | out tokens | cost |
|---|---|---|---|---|
%s`, filepath.Base(path), src, cfg.Models.TTS.Model, cfg.Models.TTS.Provider, nChunks, in, out, total, tbl.String())
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("cost: $%.6f (%d in / %d out tokens)\n", total, in, out)
	return nil
}
