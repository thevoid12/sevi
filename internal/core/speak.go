package core

import (
	"context"
	"errors"
	"fmt"
	"sevi/internal/model"
	"sync"
	"sync/atomic"
	"time"

	"sevi/internal/event"
	"sevi/internal/fetch"
	"sevi/internal/httpx"
	"sevi/internal/logx"
)

// missingSpans are the chunks still to make: the gaps between the ranges that
// are already spoken, split at the current chunk size (all of it when nothing
// is done yet).
func missingSpans(text string, done []model.SpeechSeg, chunkChars int) []model.Span {
	var todo []model.Span
	for _, g := range gaps(done, len([]rune(text))) {
		todo = append(todo, fetch.SpansIn(text, g[0], g[1], chunkChars)...)
	}
	return todo
}

// speakFailure is the first chunk that could not be made.
type speakFailure struct {
	err  error
	part int // 1-based position among the chunks of this run
}

// speak makes the chunks, several at a time. A global limit (models.tts.concurrency,
// shared by every job) keeps the number of speech calls in flight down, so two
// blogs at once do not double it. Chunks start in text order; results are kept by
// position, so the order they finish in does not matter.
//
// When a chunk cannot be made no new chunk is started, those already running are
// allowed to finish (their audio is paid for), and everything finished is
// returned along with the failure. A stop (ctx cancelled) cuts running calls short.
// If the provider rate-limits (429), the rest of the job runs one chunk at a time.
func (c *Core) speak(ctx context.Context, j *Job, sessionID string, todo []model.Span, checkpoint func(segs []model.SpeechSeg, cost float64)) (segs []model.SpeechSeg, cost float64, skipped int, fail *speakFailure) {
	if len(todo) == 0 {
		return nil, 0, 0, nil
	}
	log := logx.With(ctx, c.Log)
	var (
		mu          sync.Mutex
		wg          sync.WaitGroup
		completed   int
		rateLimited atomic.Bool
		oneAtATime  = make(chan struct{}, 1) // used by every chunk once the provider rate-limits

		// A running job is checkpointed every so often, so a platform shutdown (which
		// leaves only a few seconds) loses at most that much work.
		every    = time.Duration(c.Cfg.GetInt("models.tts.checkpoint_seconds")) * time.Second
		lastSave = time.Now()
		saving   bool
		savers   sync.WaitGroup
	)
	j.progress(fmt.Sprintf("Generating audio 0/%d", len(todo)), 0, len(todo))
	failed := func() bool { mu.Lock(); defer mu.Unlock(); return fail != nil }

dispatch:
	for i, sp := range todo {
		if ctx.Err() != nil || failed() {
			break
		}
		serial := rateLimited.Load()
		if serial {
			select {
			case oneAtATime <- struct{}{}:
			case <-ctx.Done():
				break dispatch
			}
		}
		select {
		case c.ttsSlots <- struct{}{}:
		case <-ctx.Done():
			if serial {
				<-oneAtATime
			}
			break dispatch
		}
		if failed() { // while waiting for a slot
			<-c.ttsSlots
			if serial {
				<-oneAtATime
			}
			break
		}
		wg.Add(1)
		go func(i int, sp model.Span, serial bool) {
			defer wg.Done()
			defer func() {
				<-c.ttsSlots
				if serial {
					<-oneAtATime
				}
			}()
			speech, err := c.TTS.Synth(ctx, sp.Text)
			if err != nil {
				var se *httpx.StatusError
				if errors.As(err, &se) && se.Code == 429 {
					rateLimited.Store(true)
				}
				mu.Lock()
				if fail == nil {
					fail = &speakFailure{err: err, part: i + 1}
				}
				mu.Unlock()
				return
			}
			// The call is paid for: record it even if the job was stopped a moment ago.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			callCost, err := c.recordTTS(rctx, sessionID, speech, len([]rune(sp.Text)))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if fail == nil {
					fail = &speakFailure{err: fmt.Errorf("write ledger: %w", err), part: i + 1}
				}
				return
			}
			cost += callCost
			skipped += speech.Skipped
			segs = append(segs, model.SpeechSeg{Start: sp.Start, End: sp.End, PCM: speech.PCM})
			completed++
			j.progress(fmt.Sprintf("Generating audio %d/%d", completed, len(todo)), completed, len(todo))
			if checkpoint != nil && every > 0 && !saving && time.Since(lastSave) >= every && completed < len(todo) {
				saving, lastSave = true, time.Now()
				snapshot, spent := append([]model.SpeechSeg(nil), segs...), cost
				savers.Add(1)
				go func() {
					defer savers.Done()
					checkpoint(snapshot, spent)
					mu.Lock()
					saving = false
					mu.Unlock()
				}()
			}
			log.Infow("tts chunk done", "chunk", i+1, "chunks", len(todo), "calls", max(1, len(speech.Parts)), "cost_usd", callCost)
		}(i, sp, serial)
	}
	wg.Wait()
	savers.Wait()                             // a periodic save must not land after the final one
	if fail == nil && completed < len(todo) { // stopped before every chunk was made
		fail = &speakFailure{err: context.Cause(ctx), part: completed + 1}
	}
	return segs, cost, skipped, fail
}

// speechStop is what is known when speech could not be finished.
type speechStop struct {
	hash, model, voice, textHash string
	runes                        int
	all                          []model.SpeechSeg // everything finished, this run and earlier ones
	carried, spent               float64           // cost of earlier runs, of this one
	fail                         *speakFailure
	parts                        int
}

// speechFailed ends a blog that could not be finished. Everything made is kept
// for the next try, and the person gets the audio that plays from the start of
// the article, or an error when there is none. A job the user stopped ends as
// done, not failed.
func (c *Core) speechFailed(ctx context.Context, j *Job, sessionID, title string, s speechStop) error {
	log := logx.With(ctx, c.Log).With("session", sessionID, "job", j.ID)
	stopped := errors.Is(context.Cause(ctx), ErrStopped)
	saved := c.savePartial(ctx, s.hash, s.model, s.voice, s.textHash, s.runes, s.all, s.carried+s.spent)
	prefix, covered := prefixSegs(s.all)
	pct := 100 * covered / max(s.runes, 1)

	if stopped {
		// The user stopped it: hand over what is made, even though ctx is cancelled.
		if len(prefix) > 0 {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			note := fmt.Sprintf("Stopped. Here is the first %d%% of it.", pct)
			if saved {
				note += " Send the link again to continue where it stopped."
			}
			log.Infow("blog stopped by the user, sending the partial audio", "percent_done", pct)
			if c.sendPartial(sctx, j, sessionID, title, prefix, s.carried, note) {
				return nil
			}
		}
		return ErrStopped
	}

	reason := providerMessage(s.fail.err, s.fail.part, s.parts)
	// Give the person what was made, unless the server is stopping (then nothing can be sent).
	if len(prefix) > 0 && ctx.Err() == nil {
		note := fmt.Sprintf("Only the first %d%% could be made. %s", pct, reason)
		if saved {
			note += " Send the link again to continue where it stopped."
		}
		log.Errorw("tts failed part way, sending the partial audio", "chunk", s.fail.part, "chunks", s.parts, "percent_done", pct, "err", s.fail.err)
		if c.sendPartial(ctx, j, sessionID, title, prefix, s.carried, note) {
			return nil
		}
	}
	if saved {
		reason += " What was made is saved: send the link again to continue."
	}
	return event.Wrap(reason, fmt.Errorf("chunk %d of %d (model %s): %w", s.fail.part, s.parts, s.model, s.fail.err))
}
