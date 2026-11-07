package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"sevi/internal/audio"
	"sevi/internal/config"
	"sevi/internal/event"
	"sevi/internal/fetch"
	"sevi/internal/httpx"
	"sevi/internal/logx"
	"sevi/internal/model"
	"sevi/internal/objectstore"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/store"
)

// blogJob turns a blog URL into one audio message in the session:
// audio cache -> cleaned text (cached) -> estimate/confirm -> TTS per chunk ->
// stitch -> store. Each chunk's cost is computed from tokens x the stored price
// as it is made, so the cost is known the moment the audio is.
func (c *Core) blogJob(ctx context.Context, j *Job, sessionID, pageURL string, confirmed bool) error {
	log := logx.With(ctx, c.Log).With("session", sessionID, "job", j.ID)
	cfg := c.Cfg
	ttsModel, ttsVoice := cfg.GetString("models.tts.model"), cfg.GetString("models.tts.voice")
	hash := store.URLHash(pageURL)

	// If the same blog is already being spoken, wait for it and reuse its audio
	// instead of paying for it twice.
	release, err := c.blogLocks.acquire(ctx, cacheLockKey(hash, ttsModel, ttsVoice), func() {
		log.Infow("same blog is already being made, waiting for it", "url", pageURL)
		j.progress("That one is already being made. Waiting for it…", 0, 0)
	})
	if err != nil {
		return event.Wrap("I was stopped before finishing (the server is shutting down).", err)
	}
	defer release()

	// A re-shared link with the same voice settings costs nothing.
	if ca, err := c.Store.GetAudio(ctx, hash, ttsModel, ttsVoice); err == nil {
		if have, err := c.Objects.Exists(ctx, ca.AudioKey); err != nil {
			return event.Wrap("Something went wrong on my side.", err)
		} else if have {
			title := ""
			if p, err := c.Store.GetPage(ctx, hash); err == nil {
				title = p.Title.String
			}
			msg, err := c.Store.AddMessage(ctx, model.NewMessage{SessionID: sessionID, Role: "assistant", AudioKey: ca.AudioKey, Format: "voice"})
			if err != nil {
				return event.Wrap("Something went wrong on my side.", err)
			}
			log.Infow("audio cache hit, no provider calls", "url", pageURL)
			zero := 0.0
			j.emit(event.Event{Kind: event.Notice, Message: "I already had this one. No cost."})
			j.emit(event.Event{Kind: event.Cost, Message: "Cost $0.00", CostUSD: &zero})
			j.setStatus(Done, []Part{{Type: "audio", MessageID: msg.ID, Seconds: int(ca.Seconds), Title: title, Mime: "audio/mpeg", CostUSD: &zero}})
			return nil
		}
		log.Warnw("audio cache row points at a missing file, regenerating", "key", ca.AudioKey)
	} else if !errors.Is(err, store.ErrNotFound) {
		return event.Wrap("Something went wrong on my side.", err)
	}

	text, title, err := c.pageText(ctx, j, pageURL, hash)
	if err != nil {
		return err
	}
	chunkChars := cfg.GetInt("models.tts.chunk_chars")
	runes := len([]rune(text))
	textHash := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	log.Infow("extracted", "url", pageURL, "chars", runes, "script", fetch.DetectScript(text), "chunks", len(fetch.Spans(text, chunkChars)))

	// An earlier run of this link may have stopped part way: its finished ranges
	// are kept whatever chunk size made them, and only what is missing is made now.
	// The person already agreed to the cost then, so they are not asked again.
	var done []model.SpeechSeg
	carried := 0.0 // cost of the ranges an earlier run already paid for
	if p, err := c.Store.GetPartial(ctx, hash, ttsModel, ttsVoice); err == nil && p.DoneChunks > 0 {
		if segs, err := c.loadCheckpoint(ctx, p.AudioKey, textHash, runes); err == nil {
			done, confirmed, carried = segs, true, p.CostUsd
			pct := 100 * coveredRunes(done) / max(runes, 1)
			log.Infow("continuing a partly done link", "url", pageURL, "percent_done", pct, "ranges", len(done), "cost_so_far_usd", carried)
			j.emit(event.Event{Kind: event.Notice, Message: fmt.Sprintf("Continuing: about %d%% is already made.", pct)})
		} else {
			log.Warnw("saved partial audio is unreadable, starting over", "key", p.AudioKey, "err", err)
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Warnw("could not look up partial audio, starting over", "err", err)
	}

	if !confirmed {
		est := c.estimate(ctx, len([]rune(text)))
		log.Infow("estimate", "chars", est.Chars, "usd", est.USD, "minutes", est.Minutes, "source", est.Source, "threshold_usd", c.Cfg.GetFloat64("cost.confirm_over_usd"))
		if est.USD > c.Cfg.GetFloat64("cost.confirm_over_usd") {
			if _, err := c.Store.AddPending(ctx, sessionID, "confirm_tts", mustJSON(pendingConfirm{est.Chars, est.USD, est.Minutes})); err != nil {
				return event.Wrap("Something went wrong on my side.", err)
			}
			j.emit(event.Event{Kind: event.Confirm, Message: fmt.Sprintf("This one is %s. Go ahead? (yes/no)", est)})
			j.setStatus(NeedsConfirmation, nil)
			return nil
		}
	}

	todo := missingSpans(text, done, chunkChars)
	keep := func(fresh []model.SpeechSeg, spent float64) {
		c.savePartial(ctx, hash, ttsModel, ttsVoice, textHash, runes, append(append([]model.SpeechSeg(nil), done...), fresh...), carried+spent)
	}
	fresh, spent, skipped, fail := c.speak(ctx, j, sessionID, todo, keep)
	all := append(append([]model.SpeechSeg(nil), done...), fresh...)
	if fail != nil {
		return c.speechFailed(ctx, j, sessionID, title, speechStop{
			hash: hash, model: ttsModel, voice: ttsVoice, textHash: textHash, runes: runes,
			all: all, carried: carried, spent: spent, fail: fail, parts: len(todo),
		})
	}

	j.progress("Putting the audio together", 0, 0)
	full, covered := prefixSegs(all)
	if covered != runes {
		return event.Wrap("Something went wrong on my side.", fmt.Errorf("audio covers %d of %d characters", covered, runes))
	}
	whole := audio.Stitch(full)
	mp3, err := audio.EncodeMP3(ctx, whole)
	if err != nil {
		return event.Wrap("I couldn't encode the audio (is ffmpeg installed?).", err)
	}
	seconds := int(audio.Duration(whole))
	audioKey := objectstore.Key(".mp3")
	if err := c.Objects.Put(ctx, audioKey, mp3, "audio/mpeg"); err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	if err := c.Store.PutAudio(ctx, hash, ttsModel, ttsVoice, audioKey, seconds); err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	if err := c.Store.DeletePartial(ctx, hash, ttsModel, ttsVoice); err != nil {
		log.Warnw("could not clear the saved partial audio", "err", err) // harmless: it expires with the retention period
	}
	msg, err := c.Store.AddMessage(ctx, model.NewMessage{SessionID: sessionID, Role: "assistant", AudioKey: audioKey, Format: "voice"})
	if err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	sc, err := c.Store.SessionCost(ctx, sessionID)
	if err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	cost := carried + sc.TotalUsd // this run's calls plus what the earlier run paid
	log.Infow("audio ready", "url", pageURL, "seconds", seconds, "bytes", len(mp3), "chunks", len(todo), "cost_usd", cost, "unpriced_calls", sc.UnknownCalls)

	msgText := fmt.Sprintf("Cost $%.2f", cost)
	if sc.UnknownCalls > 0 {
		msgText += fmt.Sprintf(" (%d part(s) could not be priced)", sc.UnknownCalls)
	}
	j.emit(event.Event{Kind: event.Cost, Message: msgText, CostUSD: &cost})
	j.emit(event.Event{Kind: event.Done, Message: "Audio is ready."})
	note := ""
	if skipped > 0 {
		note = fmt.Sprintf("%d sentence(s) could not be spoken by any voice model and were left out.", skipped)
	}
	j.setStatus(Done, []Part{{Type: "audio", MessageID: msg.ID, Seconds: seconds, Title: title, Mime: "audio/mpeg", CostUSD: &cost, Note: note}})
	return nil
}

// recordTTS writes a ledger row for each provider call behind one chunk (several
// when it was split or moved to a fallback model), each with its cost computed
// from tokens x the stored price, and returns the total (an unpriced call adds 0
// and its row's cost stays NULL).
func (c *Core) recordTTS(ctx context.Context, sessionID string, sp provider.Speech, chars int) (float64, error) {
	cfg := c.Cfg
	ttsProvider := cfg.GetString("models.tts.provider")
	parts := sp.Parts
	if len(parts) == 0 {
		parts = []provider.SpeechPart{{Model: cfg.GetString("models.tts.model"), GenerationID: sp.GenerationID, Chars: chars, PCM: sp.PCM}}
	}
	var total float64
	for _, part := range parts {
		seconds, tps := audio.Duration(part.PCM), cfg.GetFloat64("models.tts.audio_tokens_per_sec")
		e := model.LedgerEntry{
			SessionID: sessionID, Role: "tts", Provider: ttsProvider, Model: part.Model,
			GenerationID: part.GenerationID, Chars: part.Chars,
		}
		if p, err := c.Prices.Get(ctx, ttsProvider, part.Model); err != nil {
			e.InputUnits, e.OutputUnits = pricing.TTSTokens(part.Chars, seconds, tps)
			logx.With(ctx, c.Log).Errorw("no price for the tts model, cost left unknown", "model", part.Model, "err", err)
		} else {
			// What the price is per (token, character or second) decides how the call is counted.
			unit := pricing.UnitFor(config.PricedPer(cfg, part.Model), p)
			e.InputUnits, e.OutputUnits = pricing.TTSUnits(unit, part.Chars, seconds, tps)
			cost := pricing.TTSCost(p, e.InputUnits, e.OutputUnits)
			e.CostUSD = &cost
			total += cost
		}
		if _, err := c.Store.AddLedger(ctx, e); err != nil {
			return 0, fmt.Errorf("write ledger: %w", err)
		}
	}
	return total, nil
}

// pageText returns the cleaned article text and its title, cached by URL.
func (c *Core) pageText(ctx context.Context, j *Job, pageURL, hash string) (text, title string, err error) {
	log := logx.With(ctx, c.Log)
	if p, err := c.Store.GetPage(ctx, hash); err == nil {
		log.Infow("page cache hit", "url", pageURL)
		return p.Text, p.Title.String, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", "", event.Wrap("Something went wrong on my side.", err)
	}
	j.progress("Reading the article", 0, 0)
	md, err := c.Markdown(ctx, pageURL)
	if err != nil {
		return "", "", event.Wrap("I couldn't read that page. The site may block readers, or the link may be wrong.", fmt.Errorf("fetch %s: %w", pageURL, err))
	}
	text, title = fetch.ToSpeakable(md), fetch.Title(md)
	if text == "" {
		return "", "", event.Wrap("I couldn't find any readable text on that page.", fmt.Errorf("empty text from %s", pageURL))
	}
	if err := c.Store.PutPage(ctx, pageURL, title, text); err != nil {
		log.Errorw("could not cache page text", "url", pageURL, "err", err)
	}
	return text, title, nil
}

// sendPartial finishes the job with the audio made so far, marked as incomplete,
// instead of failing it. It is not put in the audio cache, so sharing the link
// again continues from the saved parts. It reports false if it could not.
func (c *Core) sendPartial(ctx context.Context, j *Job, sessionID, title string, pcm [][]byte, carried float64, note string) bool {
	log := logx.With(ctx, c.Log).With("session", sessionID, "job", j.ID)
	all := audio.Stitch(pcm)
	mp3, err := audio.EncodeMP3(ctx, all)
	var msg model.Message
	var sc model.SessionCost
	if err == nil {
		key := objectstore.Key(".mp3")
		if err = c.Objects.Put(ctx, key, mp3, "audio/mpeg"); err == nil {
			if msg, err = c.Store.AddMessage(ctx, model.NewMessage{SessionID: sessionID, Role: "assistant", AudioKey: key, Format: "voice"}); err == nil {
				sc, err = c.Store.SessionCost(ctx, sessionID)
			}
		}
	}
	if err != nil {
		log.Errorw("could not send the partial audio", "err", err)
		return false
	}
	cost := carried + sc.TotalUsd
	seconds := int(audio.Duration(all))
	log.Infow("partial audio sent", "seconds", seconds, "bytes", len(mp3), "cost_usd", cost)
	j.emit(event.Event{Kind: event.Cost, Message: fmt.Sprintf("Cost $%.2f", cost), CostUSD: &cost})
	j.emit(event.Event{Kind: event.Done, Message: "Part of the audio is ready."})
	j.setStatus(Done, []Part{{Type: "audio", MessageID: msg.ID, Seconds: seconds, Title: title, Mime: "audio/mpeg", CostUSD: &cost, Note: note}})
	return true
}

// loadCheckpoint reads a saved checkpoint back into its finished ranges.
func (c *Core) loadCheckpoint(ctx context.Context, key, textHash string, textLen int) ([]model.SpeechSeg, error) {
	data, err := c.Objects.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return decodeCheckpoint(ctx, data, textHash, textLen)
}

// savePartial keeps every finished range of a half-done blog as one compressed
// object plus a database row, so sharing the link again makes only what is
// missing. The row's total and done counts are characters of the text. It uses
// its own short deadline and ignores cancellation, so it still runs when the
// server is shutting down. It reports whether anything was saved.
func (c *Core) savePartial(ctx context.Context, hash, ttsModel, voice, textHash string, textLen int, segs []model.SpeechSeg, costUSD float64) bool {
	if len(segs) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	log := logx.With(ctx, c.Log)
	key := objectstore.Key(".ckpt")
	data, err := encodeCheckpoint(ctx, textHash, textLen, segs)
	if err == nil {
		err = c.Objects.Put(ctx, key, data, "application/octet-stream")
	}
	covered := coveredRunes(segs)
	if err == nil {
		err = c.Store.PutPartial(ctx, hash, ttsModel, voice, textLen, covered, key, costUSD)
	}
	if err != nil {
		log.Errorw("could not save partial audio", "err", err)
		return false
	}
	log.Infow("partial audio kept", "key", key, "percent_done", 100*covered/max(textLen, 1), "ranges", len(mergeRanges(segs)), "cost_so_far_usd", costUSD, "bytes", len(data))
	return true
}

// cacheLockKey identifies one blog + voice settings, so two requests for the
// same audio wait on each other. It is not an object key (those are timestamps).
func cacheLockKey(hash, model, voice string) string {
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
