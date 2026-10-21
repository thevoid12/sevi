package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"

	"sevi/internal/audio"
	"sevi/internal/blob"
	"sevi/internal/config"
	"sevi/internal/fetch"
	"sevi/internal/provider"
	"sevi/internal/store"
)

// runTTS is the blog-to-audio flow: URL -> clean text (cached) -> TTS per chunk
// -> one MP3 (cached) + cost. Every successful provider call is a ledger row.
// If a chunk fails after retries the job stops with an error naming the chunk,
// and the audio of the chunks that did succeed is kept as <slug>.partial.mp3.
// refresh bypasses both caches.
func runTTS(ctx context.Context, log *zap.SugaredLogger, cfg *config.Config, st *store.Store, bl blob.Store, pageURL string, refresh bool) error {
	ttsCfg := cfg.Models.TTS
	key, err := cfg.ProviderKey(ttsCfg.Provider)
	if err != nil {
		return err
	}
	sess, err := st.CreateSession(ctx, "blog", pageURL)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	log = log.With("session", sess.ID)
	hash := store.URLHash(pageURL)

	if err := os.MkdirAll(cfg.App.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	base := filepath.Join(cfg.App.OutputDir, fetch.Slug(pageURL))

	// Audio cache: a re-shared link with the same voice settings costs nothing.
	if !refresh {
		if ca, err := st.GetAudio(ctx, hash, ttsCfg.Model, ttsCfg.Voice); err == nil {
			if mp3, err := bl.Get(ctx, ca.AudioKey); err == nil {
				out := base + ".mp3"
				if err := os.WriteFile(out, mp3, 0o644); err != nil {
					return err
				}
				log.Infow("audio cache hit, no provider calls", "url", pageURL, "path", out, "seconds", ca.Seconds)
				fmt.Println("wrote", out, "(cached, cost $0)")
				return nil
			} else if !errors.Is(err, blob.ErrNotFound) {
				return err
			}
			log.Warnw("audio cache row points at a missing file, regenerating", "key", ca.AudioKey)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}

	text, err := pageText(ctx, log, cfg, st, pageURL, hash, refresh)
	if err != nil {
		return err
	}
	chunks := fetch.Chunk(text, ttsCfg.ChunkChars)
	log.Infow("extracted", "url", pageURL, "chars", len([]rune(text)), "script", fetch.DetectScript(text), "chunks", len(chunks))
	if len(chunks) == 0 {
		return fmt.Errorf("no speakable text found at %s", pageURL)
	}

	prov := cfg.Provider(ttsCfg.Provider)
	tts := &provider.OpenRouterTTS{
		BaseURL: prov.BaseURL, Key: key, Model: ttsCfg.Model, Voice: ttsCfg.Voice,
		Retries: cfg.App.Retries, Log: log,
	}

	var pcm [][]byte
	var ledgerIDs []string
	var genIDs []string
	for i, c := range chunks {
		log.Infow("tts chunk", "n", i+1, "of", len(chunks), "chars", len([]rune(c)))
		sp, err := tts.Synth(ctx, c)
		if err != nil {
			if len(pcm) > 0 {
				savePartial(ctx, log, base+".partial.mp3", pcm)
			}
			return fmt.Errorf("chunk %d of %d failed (model %s): %w", i+1, len(chunks), ttsCfg.Model, err)
		}
		id, err := st.AddLedger(ctx, store.LedgerEntry{
			SessionID: sess.ID, Role: "tts", Provider: ttsCfg.Provider, Model: ttsCfg.Model,
			GenerationID: sp.GenerationID, Chars: len([]rune(c)),
		})
		if err != nil {
			return fmt.Errorf("write ledger: %w", err)
		}
		pcm = append(pcm, sp.PCM)
		ledgerIDs = append(ledgerIDs, id)
		genIDs = append(genIDs, sp.GenerationID)
	}

	all := audio.Stitch(pcm)
	mp3, err := audio.EncodeMP3(ctx, all)
	if err != nil {
		return err
	}
	seconds := int(audio.Duration(all))
	audioKey := audioKeyFor(hash, ttsCfg.Model, ttsCfg.Voice)
	if err := bl.Put(ctx, audioKey, mp3); err != nil {
		return fmt.Errorf("store audio: %w", err)
	}
	if err := st.PutAudio(ctx, hash, ttsCfg.Model, ttsCfg.Voice, audioKey, seconds); err != nil {
		return fmt.Errorf("cache audio: %w", err)
	}
	out := base + ".mp3"
	if err := os.WriteFile(out, mp3, 0o644); err != nil {
		return err
	}
	log.Infow("wrote audio", "path", out, "seconds", seconds, "bytes", len(mp3))
	fmt.Println("wrote", out)

	return recordCosts(ctx, log, cfg, st, sess.ID, key, base+".cost.md", pageURL, ledgerIDs, genIDs)
}

func audioKeyFor(hash, model, voice string) string {
	v := sha256.Sum256([]byte(model + "|" + voice))
	return fmt.Sprintf("audio/%s-%x.mp3", hash[:16], v[:4])
}

// pageText returns the cleaned article text, from cache unless refresh.
func pageText(ctx context.Context, log *zap.SugaredLogger, cfg *config.Config, st *store.Store, pageURL, hash string, refresh bool) (string, error) {
	if !refresh {
		if p, err := st.GetPage(ctx, hash); err == nil {
			log.Infow("page cache hit", "url", pageURL)
			return p.Text, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	jinaURL, jinaKey := "https://r.jina.ai", ""
	if j, ok := cfg.Providers["jina"]; ok {
		jinaURL, jinaKey = j.BaseURL, os.Getenv(j.KeyEnv)
	}
	md, err := fetch.Markdown(ctx, jinaURL, jinaKey, pageURL)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", pageURL, err)
	}
	text := fetch.ToSpeakable(md)
	if text != "" {
		if err := st.PutPage(ctx, pageURL, text); err != nil {
			log.Errorw("could not cache page text", "url", pageURL, "err", err)
		}
	}
	return text, nil
}

func savePartial(ctx context.Context, log *zap.SugaredLogger, path string, pcm [][]byte) {
	mp3, err := audio.EncodeMP3(ctx, audio.Stitch(pcm))
	if err == nil {
		err = os.WriteFile(path, mp3, 0o644)
	}
	if err != nil {
		log.Errorw("could not save partial audio", "path", path, "err", err)
		return
	}
	log.Infow("partial audio kept", "path", path, "chunks_done", len(pcm))
}

// recordCosts looks up each call's usage at the provider, fills in the ledger
// rows, and writes a markdown report. A failed lookup leaves that row's cost
// NULL (unknown) so it can be backfilled later.
func recordCosts(ctx context.Context, log *zap.SugaredLogger, cfg *config.Config, st *store.Store, sessionID string, key, path, src string, ledgerIDs []string, genIDs []string) error {
	prov := cfg.Provider(cfg.Models.TTS.Provider)

	usages := make([]provider.Usage, len(genIDs))
	errs := make([]error, len(genIDs))
	var wg sync.WaitGroup
	for i, id := range genIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			usages[i], errs[i] = provider.OpenRouterUsage(ctx, prov.BaseURL, key, id)
		}()
	}
	wg.Wait()

	rows := ""
	for i, u := range usages {
		if errs[i] != nil {
			log.Errorw("cost lookup failed, ledger cost left unknown", "generation", genIDs[i], "ledger_id", ledgerIDs[i], "err", errs[i])
			rows += fmt.Sprintf("| %d | %s | ? | ? | ? |\n", i+1, genIDs[i])
			continue
		}
		if err := st.SetLedgerUsage(ctx, ledgerIDs[i], u.PromptTokens, u.CompletionTokens, u.CostUSD); err != nil {
			return fmt.Errorf("update ledger: %w", err)
		}
		rows += fmt.Sprintf("| %d | %s | %d | %d | $%.6f |\n", i+1, genIDs[i], u.PromptTokens, u.CompletionTokens, u.CostUSD)
	}

	sc, err := st.SessionCost(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session cost: %w", err)
	}
	ledger, err := st.ListLedger(ctx, sessionID)
	if err != nil {
		return err
	}
	var in, out int64
	for _, r := range ledger {
		in += r.InputUnits.Int64
		out += r.OutputUnits.Int64
	}
	log.Infow("cost", "total_usd", sc.TotalUsd, "calls", sc.Calls, "unknown_calls", sc.UnknownCalls, "input_tokens", in, "output_tokens", out, "model", cfg.Models.TTS.Model)

	md := fmt.Sprintf(`# Cost: %s

- Source: %s
- Session: %s
- Model: %s (via %s)
- Chunks: %d
- Input tokens: %d
- Output (audio) tokens: %d
- **Total: $%.6f**%s

| # | generation id | in tokens | out tokens | cost |
|---|---|---|---|---|
%s`, filepath.Base(path), src, sessionID, cfg.Models.TTS.Model, cfg.Models.TTS.Provider, len(genIDs), in, out, sc.TotalUsd, unknownNote(sc.UnknownCalls), rows)
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("cost: $%.6f (%d in / %d out tokens)\n", sc.TotalUsd, in, out)
	fmt.Println("wrote", path)
	return nil
}

func unknownNote(n int64) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d call(s) with unknown cost)", n)
}
