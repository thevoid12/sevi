package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"sevi/internal/audio"
	"sevi/internal/blob"
	"sevi/internal/event"
	"sevi/internal/fetch"
	"sevi/internal/httpx"
	"sevi/internal/logx"
	"sevi/internal/provider"
	"sevi/internal/store"
)

// blogJob turns a blog URL into one audio message in the session:
// audio cache -> cleaned text (cached) -> estimate/confirm -> TTS per chunk ->
// stitch -> store. Cost lookup runs after the audio is delivered.
func (c *Core) blogJob(ctx context.Context, j *Job, sessionID, pageURL string, confirmed bool) error {
	log := logx.With(ctx, c.Log).With("session", sessionID, "job", j.ID)
	tts := c.Cfg.Models.TTS
	hash := store.URLHash(pageURL)

	// A re-shared link with the same voice settings costs nothing.
	if ca, err := c.Store.GetAudio(ctx, hash, tts.Model, tts.Voice); err == nil {
		if _, err := c.Blob.Get(ctx, ca.AudioKey); err == nil {
			msg, err := c.Store.AddMessage(ctx, sessionID, "assistant", "", ca.AudioKey)
			if err != nil {
				return event.Wrap("Something went wrong on my side.", err)
			}
			log.Infow("audio cache hit, no provider calls", "url", pageURL)
			zero := 0.0
			j.emit(event.Event{Kind: event.Notice, Message: "I already had this one. No cost."})
			j.emit(event.Event{Kind: event.Cost, Message: "Cost $0.00", CostUSD: &zero})
			j.setStatus(Done, []Part{{Type: "audio", MessageID: msg.ID, Seconds: int(ca.Seconds)}})
			return nil
		} else if !errors.Is(err, blob.ErrNotFound) {
			return event.Wrap("Something went wrong on my side.", err)
		}
		log.Warnw("audio cache row points at a missing file, regenerating", "key", ca.AudioKey)
	} else if !errors.Is(err, store.ErrNotFound) {
		return event.Wrap("Something went wrong on my side.", err)
	}

	text, err := c.pageText(ctx, j, pageURL, hash)
	if err != nil {
		return err
	}
	chunks := fetch.Chunk(text, tts.ChunkChars)
	log.Infow("extracted", "url", pageURL, "chars", len([]rune(text)), "script", fetch.DetectScript(text), "chunks", len(chunks))

	if !confirmed {
		est, err := c.estimate(ctx, len([]rune(text)))
		if err != nil {
			return event.Wrap("Something went wrong on my side.", err)
		}
		log.Infow("estimate", "chars", est.Chars, "usd", est.USD, "minutes", est.Minutes, "source", est.Source, "threshold_usd", c.Cfg.Cost.ConfirmOverUSD)
		if est.USD > c.Cfg.Cost.ConfirmOverUSD {
			if _, err := c.Store.AddPending(ctx, sessionID, "confirm_tts", mustJSON(pendingConfirm{est.Chars, est.USD, est.Minutes})); err != nil {
				return event.Wrap("Something went wrong on my side.", err)
			}
			j.emit(event.Event{Kind: event.Confirm, Message: fmt.Sprintf("This one is %s. Go ahead? (yes/no)", est)})
			j.setStatus(NeedsConfirmation, nil)
			return nil
		}
	}

	var pcm [][]byte
	var ledgerIDs, genIDs []string
	for i, ch := range chunks {
		j.progress(fmt.Sprintf("Generating audio %d/%d", i+1, len(chunks)), i+1, len(chunks))
		sp, err := c.TTS.Synth(ctx, ch)
		if err != nil {
			c.savePartial(ctx, sessionID, pcm)
			return event.Wrap(providerMessage(err, i+1, len(chunks)), fmt.Errorf("chunk %d of %d (model %s): %w", i+1, len(chunks), tts.Model, err))
		}
		id, err := c.Store.AddLedger(ctx, store.LedgerEntry{
			SessionID: sessionID, Role: "tts", Provider: tts.Provider, Model: tts.Model,
			GenerationID: sp.GenerationID, Chars: len([]rune(ch)),
		})
		if err != nil {
			return event.Wrap("Something went wrong on my side.", err)
		}
		pcm = append(pcm, sp.PCM)
		ledgerIDs = append(ledgerIDs, id)
		genIDs = append(genIDs, sp.GenerationID)
	}

	j.progress("Putting the audio together", 0, 0)
	all := audio.Stitch(pcm)
	mp3, err := audio.EncodeMP3(ctx, all)
	if err != nil {
		return event.Wrap("I couldn't encode the audio (is ffmpeg installed?).", err)
	}
	seconds := int(audio.Duration(all))
	audioKey := audioKeyFor(hash, tts.Model, tts.Voice)
	if err := c.Blob.Put(ctx, audioKey, mp3); err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	if err := c.Store.PutAudio(ctx, hash, tts.Model, tts.Voice, audioKey, seconds); err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	msg, err := c.Store.AddMessage(ctx, sessionID, "assistant", "", audioKey)
	if err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	log.Infow("audio ready", "url", pageURL, "seconds", seconds, "bytes", len(mp3), "chunks", len(chunks))

	j.setCostPending(true)
	j.emit(event.Event{Kind: event.Done, Message: "Audio is ready."})
	j.setStatus(Done, []Part{{Type: "audio", MessageID: msg.ID, Seconds: seconds}})

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.resolveCosts(ctx, j, sessionID, ledgerIDs, genIDs)
	}()
	return nil
}

// pageText returns the cleaned article text, cached by URL.
func (c *Core) pageText(ctx context.Context, j *Job, pageURL, hash string) (string, error) {
	log := logx.With(ctx, c.Log)
	if p, err := c.Store.GetPage(ctx, hash); err == nil {
		log.Infow("page cache hit", "url", pageURL)
		return p.Text, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", event.Wrap("Something went wrong on my side.", err)
	}
	j.progress("Reading the article", 0, 0)
	md, err := c.Markdown(ctx, pageURL)
	if err != nil {
		return "", event.Wrap("I couldn't read that page. The site may block readers, or the link may be wrong.", fmt.Errorf("fetch %s: %w", pageURL, err))
	}
	text := fetch.ToSpeakable(md)
	if text == "" {
		return "", event.Wrap("I couldn't find any readable text on that page.", fmt.Errorf("empty text from %s", pageURL))
	}
	if err := c.Store.PutPage(ctx, pageURL, text); err != nil {
		log.Errorw("could not cache page text", "url", pageURL, "err", err)
	}
	return text, nil
}

// savePartial keeps the audio of the chunks that did succeed, for later use.
func (c *Core) savePartial(ctx context.Context, sessionID string, pcm [][]byte) {
	if len(pcm) == 0 {
		return
	}
	log := logx.With(ctx, c.Log).With("session", sessionID)
	mp3, err := audio.EncodeMP3(ctx, audio.Stitch(pcm))
	if err == nil {
		err = c.Blob.Put(ctx, "partial/"+sessionID+".mp3", mp3)
	}
	if err != nil {
		log.Errorw("could not save partial audio", "err", err)
		return
	}
	log.Infow("partial audio kept", "key", "partial/"+sessionID+".mp3", "chunks_done", len(pcm))
}

// resolveCosts looks up each call's usage at the provider (slow: the record
// can take a minute to appear), fills in the ledger, and emits the cost event.
// A failed lookup leaves that row's cost NULL (unknown) for later backfill.
func (c *Core) resolveCosts(ctx context.Context, j *Job, sessionID string, ledgerIDs, genIDs []string) {
	log := logx.With(ctx, c.Log).With("session", sessionID, "job", j.ID)
	usages := make([]provider.Usage, len(genIDs))
	errs := make([]error, len(genIDs))
	var wg sync.WaitGroup
	for i, id := range genIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			usages[i], errs[i] = c.Usage(ctx, id)
		}()
	}
	wg.Wait()

	for i, u := range usages {
		if errs[i] != nil {
			log.Errorw("cost lookup failed, ledger cost left unknown", "generation", genIDs[i], "ledger_id", ledgerIDs[i], "err", errs[i])
			continue
		}
		if err := c.Store.SetLedgerUsage(ctx, ledgerIDs[i], u.PromptTokens, u.CompletionTokens, u.CostUSD); err != nil {
			log.Errorw("could not update ledger", "ledger_id", ledgerIDs[i], "err", err)
		}
	}
	sc, err := c.Store.SessionCost(ctx, sessionID)
	if err != nil {
		log.Errorw("could not read session cost", "err", err)
		j.setCostPending(false)
		return
	}
	log.Infow("cost", "total_usd", sc.TotalUsd, "calls", sc.Calls, "unknown_calls", sc.UnknownCalls, "model", c.Cfg.Models.TTS.Model)
	msg := fmt.Sprintf("Cost $%.2f", sc.TotalUsd)
	if sc.UnknownCalls > 0 {
		msg += fmt.Sprintf(" (%d part(s) not priced yet)", sc.UnknownCalls)
	}
	total := sc.TotalUsd
	j.emit(event.Event{Kind: event.Cost, Message: msg, CostUSD: &total})
	j.setCostPending(false)
}

func audioKeyFor(hash, model, voice string) string {
	v := sha256.Sum256([]byte(model + "|" + voice))
	return fmt.Sprintf("audio/%s-%x.mp3", hash[:16], v[:4])
}

// providerMessage maps a provider failure to text that is safe to show.
func providerMessage(err error, part, total int) string {
	var se *httpx.StatusError
	switch {
	case errors.Is(err, context.Canceled):
		return "I was stopped before finishing (the server is shutting down)."
	case errors.As(err, &se) && (se.Code == 401 || se.Code == 403):
		return "The speech service rejected my API key."
	case errors.As(err, &se) && se.Code == 402:
		return "The speech service says the account is out of credit."
	case errors.As(err, &se) && se.Code == 429:
		return fmt.Sprintf("The speech service is rate limiting me (stopped at part %d of %d). Try again in a minute.", part, total)
	case errors.As(err, &se) && se.Code >= 500:
		return fmt.Sprintf("The speech service is having problems (stopped at part %d of %d). Try again shortly.", part, total)
	default:
		return fmt.Sprintf("Audio generation failed at part %d of %d.", part, total)
	}
}
