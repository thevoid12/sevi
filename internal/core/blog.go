package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"sevi/internal/audio"
	"sevi/internal/blob"
	"sevi/internal/event"
	"sevi/internal/fetch"
	"sevi/internal/httpx"
	"sevi/internal/logx"
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
	tts := c.Cfg.Models.TTS
	hash := store.URLHash(pageURL)

	// A re-shared link with the same voice settings costs nothing.
	if ca, err := c.Store.GetAudio(ctx, hash, tts.Model, tts.Voice); err == nil {
		if _, err := c.Blob.Get(ctx, ca.AudioKey); err == nil {
			title := ""
			if p, err := c.Store.GetPage(ctx, hash); err == nil {
				title = p.Title.String
			}
			msg, err := c.Store.AddMessage(ctx, store.NewMessage{SessionID: sessionID, Role: "assistant", AudioKey: ca.AudioKey, Format: "voice"})
			if err != nil {
				return event.Wrap("Something went wrong on my side.", err)
			}
			log.Infow("audio cache hit, no provider calls", "url", pageURL)
			zero := 0.0
			j.emit(event.Event{Kind: event.Notice, Message: "I already had this one. No cost."})
			j.emit(event.Event{Kind: event.Cost, Message: "Cost $0.00", CostUSD: &zero})
			j.setStatus(Done, []Part{{Type: "audio", MessageID: msg.ID, Seconds: int(ca.Seconds), Title: title, Mime: "audio/mpeg", CostUSD: &zero}})
			return nil
		} else if !errors.Is(err, blob.ErrNotFound) {
			return event.Wrap("Something went wrong on my side.", err)
		}
		log.Warnw("audio cache row points at a missing file, regenerating", "key", ca.AudioKey)
	} else if !errors.Is(err, store.ErrNotFound) {
		return event.Wrap("Something went wrong on my side.", err)
	}

	text, title, err := c.pageText(ctx, j, pageURL, hash)
	if err != nil {
		return err
	}
	chunks := fetch.Chunk(text, tts.ChunkChars)
	log.Infow("extracted", "url", pageURL, "chars", len([]rune(text)), "script", fetch.DetectScript(text), "chunks", len(chunks))

	if !confirmed {
		est := c.estimate(ctx, len([]rune(text)))
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
	var total float64
	for i, ch := range chunks {
		j.progress(fmt.Sprintf("Generating audio %d/%d", i+1, len(chunks)), i+1, len(chunks))
		sp, err := c.TTS.Synth(ctx, ch)
		if err != nil {
			c.savePartial(ctx, sessionID, pcm)
			return event.Wrap(providerMessage(err, i+1, len(chunks)), fmt.Errorf("chunk %d of %d (model %s): %w", i+1, len(chunks), tts.Model, err))
		}
		cost, err := c.recordTTS(ctx, sessionID, sp, len([]rune(ch)))
		if err != nil {
			return event.Wrap("Something went wrong on my side.", err)
		}
		total += cost
		pcm = append(pcm, sp.PCM)
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
	msg, err := c.Store.AddMessage(ctx, store.NewMessage{SessionID: sessionID, Role: "assistant", AudioKey: audioKey, Format: "voice"})
	if err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	sc, err := c.Store.SessionCost(ctx, sessionID)
	if err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	cost := sc.TotalUsd
	log.Infow("audio ready", "url", pageURL, "seconds", seconds, "bytes", len(mp3), "chunks", len(chunks), "cost_usd", cost, "unpriced_calls", sc.UnknownCalls)

	msgText := fmt.Sprintf("Cost $%.2f", cost)
	if sc.UnknownCalls > 0 {
		msgText += fmt.Sprintf(" (%d part(s) could not be priced)", sc.UnknownCalls)
	}
	j.emit(event.Event{Kind: event.Cost, Message: msgText, CostUSD: &cost})
	j.emit(event.Event{Kind: event.Done, Message: "Audio is ready."})
	j.setStatus(Done, []Part{{Type: "audio", MessageID: msg.ID, Seconds: seconds, Title: title, Mime: "audio/mpeg", CostUSD: &cost}})
	return nil
}

// recordTTS writes the ledger row for one synthesized chunk with its cost
// computed from tokens x the stored price, and returns that cost (0 if the
// price is unknown, in which case the row's cost stays NULL).
func (c *Core) recordTTS(ctx context.Context, sessionID string, sp provider.Speech, chars int) (float64, error) {
	tts := c.Cfg.Models.TTS
	in, out := pricing.TTSTokens(chars, audio.Duration(sp.PCM), tts.AudioTokensPerSec)
	e := store.LedgerEntry{
		SessionID: sessionID, Role: "tts", Provider: tts.Provider, Model: tts.Model,
		GenerationID: sp.GenerationID, Chars: chars, InputUnits: in, OutputUnits: out,
	}
	var cost float64
	if p, err := c.Prices.Get(ctx, tts.Provider, tts.Model); err != nil {
		logx.With(ctx, c.Log).Errorw("no price for the tts model, cost left unknown", "model", tts.Model, "err", err)
	} else {
		cost = pricing.TTSCost(p, in, out)
		e.CostUSD = &cost
	}
	if _, err := c.Store.AddLedger(ctx, e); err != nil {
		return 0, fmt.Errorf("write ledger: %w", err)
	}
	return cost, nil
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
