package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"sevi/internal/fetch"
	"sevi/internal/httpx"
	"sevi/internal/logx"
)

// maxSplitDepth: a chunk that fails is cut into halves, and a half that fails
// into quarters. Quarters are not cut further; they go to the fallback models.
const maxSplitDepth = 2

// A chunk with more than maxSkipped sentences nothing could speak fails instead
// of being delivered with holes in it.
const maxSkipped = 3

// After primaryStreak whole chunks in a row failed on the primary, it is
// skipped for primaryCooldown and chunks go to the fallbacks first, so a model
// that is down does not cost a few minutes per chunk.
const (
	primaryStreak   = 2
	primaryCooldown = 10 * time.Minute
)

// Resilient wraps a TTS so one stubborn chunk does not fail the whole job.
//
//  1. The chunk is tried on Primary with its normal retries.
//  2. If the provider keeps failing (a 5xx or an empty audio stream, which
//     usually depends on the text), the chunk is cut at a sentence into two
//     halves, each tried once. A half that fails is cut again into quarters,
//     each tried once. Pieces are not retried: that would take too long.
//  3. A quarter that still fails is tried once on each fallback model in order.
//
// Only the piece that failed is cut or moved; the rest of the chunk stays on the
// primary model and voice. A fallback piece has another voice.
type Resilient struct {
	Primary   *OpenRouterTTS
	Fallbacks []*OpenRouterTTS
	Log       *zap.SugaredLogger

	mu        sync.Mutex
	streak    int              // whole chunks in a row that failed on the primary
	skipUntil time.Time        // while in the future, chunks go to the fallbacks first
	now       func() time.Time // tests only
}

func (r *Resilient) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// primaryDown reports whether the primary is being skipped.
func (r *Resilient) primaryDown() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clock().Before(r.skipUntil)
}

// note records how a whole chunk fared on the primary.
func (r *Resilient) note(ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		r.streak = 0
		return
	}
	if r.streak++; r.streak >= primaryStreak {
		r.skipUntil = r.clock().Add(primaryCooldown)
		r.streak = 0
	}
}

func (r *Resilient) Synth(ctx context.Context, text string) (Speech, error) {
	if len(r.Fallbacks) > 0 && r.primaryDown() {
		logx.With(ctx, r.Log).Warnw("tts primary model is failing, using the fallbacks first", "model", r.Primary.Model, "chars", len([]rune(text)))
		if sp, err := r.fallback(ctx, text, errors.New("primary model skipped")); err == nil {
			return sp, nil
		} else if ctx.Err() != nil {
			return Speech{}, err
		}
	}
	sp, err := r.Primary.Synth(ctx, text) // the whole chunk gets the configured retries; split pieces get none
	if err == nil {
		r.note(true)
		return sp, nil
	}
	if !splittable(ctx, err) {
		return sp, err
	}
	r.note(false)
	a, b, ok := fetch.SplitHalf(text)
	if !ok {
		sp, err = r.fallback(ctx, text, err)
		r.recovered(ctx, sp, err)
		return sp, err
	}
	logx.With(ctx, r.Log).Warnw("tts chunk failed, splitting it in two", "model", r.Primary.Model, "chars", len([]rune(text)), "err", err)
	sp, err = r.join(ctx, 1, a, b)
	if err == nil && (len(sp.PCM) == 0 || sp.Skipped > maxSkipped) {
		// Too much was left out to call the chunk done: better to stop and let the person retry.
		err = fmt.Errorf("%d sentence(s) of this chunk could not be spoken by any model", sp.Skipped)
		sp = Speech{}
	}
	r.recovered(ctx, sp, err)
	return sp, err
}

// join speaks the pieces in order and returns them as one Speech.
func (r *Resilient) join(ctx context.Context, depth int, pieces ...string) (Speech, error) {
	var out Speech
	for _, piece := range pieces {
		sp, err := r.piece(ctx, piece, depth)
		if err != nil {
			return Speech{}, err
		}
		if len(sp.PCM) > 0 { // a piece that was only skipped made no call to record
			out.PCM = append(out.PCM, sp.PCM...)
			out.Parts = append(out.Parts, sp.parts(r.Primary.Model, piece)...)
		}
		out.Skipped += sp.Skipped
		if sp.cause != nil {
			out.cause = sp.cause
		}
		// Two pieces in a row with nothing spoken at all look like an outage, not a bad sentence: stop spending calls.
		if len(out.PCM) == 0 && out.Skipped >= 2 {
			return Speech{}, out.cause
		}
	}
	return out, nil
}

// piece tries text once on the primary model, then splits it (depth < max) or
// moves it to the fallbacks.
func (r *Resilient) piece(ctx context.Context, text string, depth int) (Speech, error) {
	once := *r.Primary
	once.Retries = 0
	sp, err := once.Synth(ctx, text)
	if err == nil || !splittable(ctx, err) {
		return sp, err
	}
	if depth < maxSplitDepth {
		if a, b, ok := fetch.SplitHalf(text); ok {
			logx.With(ctx, r.Log).Warnw("tts piece failed, splitting it again", "model", r.Primary.Model, "chars", len([]rune(text)), "err", err)
			return r.join(ctx, depth+1, a, b)
		}
	}
	sp, ferr := r.fallback(ctx, text, err)
	if ferr == nil || ctx.Err() != nil {
		return sp, ferr
	}
	return r.bySentence(ctx, text, ferr)
}

// bySentence is the last resort for a piece no model could speak: each
// sentence is tried once on the primary and then on the fallbacks, and a
// sentence nothing can speak is left out (counted in Skipped) rather than
// losing the whole piece. It fails only when no sentence could be spoken.
func (r *Resilient) bySentence(ctx context.Context, text string, cause error) (Speech, error) {
	sentences := fetch.Sentences(text)
	if len(sentences) < 2 { // a single sentence nothing can speak
		logx.With(ctx, r.Log).Errorw("tts sentence skipped, no model could speak it", "chars", len([]rune(text)), "err", cause)
		return Speech{Skipped: 1, cause: cause}, nil
	}
	logx.With(ctx, r.Log).Warnw("tts piece failed on every model, speaking it sentence by sentence", "sentences", len(sentences), "chars", len([]rune(text)))
	var out Speech
	for _, s := range sentences {
		once := *r.Primary
		once.Retries = 0
		sp, err := once.Synth(ctx, s)
		if err != nil && splittable(ctx, err) {
			sp, err = r.fallback(ctx, s, err)
		}
		if ctx.Err() != nil {
			return Speech{}, ctx.Err()
		}
		if err != nil {
			out.Skipped++
			logx.With(ctx, r.Log).Errorw("tts sentence skipped, no model could speak it", "chars", len([]rune(s)), "err", err)
			continue
		}
		out.PCM = append(out.PCM, sp.PCM...)
		out.Parts = append(out.Parts, sp.parts(r.Primary.Model, s)...)
	}
	if len(out.PCM) == 0 {
		return Speech{}, cause
	}
	return out, nil
}

// fallback tries each fallback model once. It returns the primary error when
// there is none or all fail.
func (r *Resilient) fallback(ctx context.Context, text string, cause error) (Speech, error) {
	for _, fb := range r.Fallbacks {
		one := *fb
		one.Retries = 0
		logx.With(ctx, r.Log).Warnw("tts piece failed on the primary model, trying a fallback", "from", r.Primary.Model, "to", fb.Model, "voice", fb.Voice, "chars", len([]rune(text)))
		sp, err := one.Synth(ctx, text)
		if err == nil {
			logx.With(ctx, r.Log).Infow("tts fallback worked", "model", fb.Model, "chars", len([]rune(text)))
			sp.Parts = sp.parts(fb.Model, text)
			return sp, nil
		}
		if ctx.Err() != nil {
			return Speech{}, ctx.Err()
		}
		logx.With(ctx, r.Log).Warnw("tts fallback failed", "model", fb.Model, "err", err)
	}
	return Speech{}, fmt.Errorf("%w (also tried %d fallback model(s))", cause, len(r.Fallbacks))
}

// recovered logs how a chunk that failed on the first try ended up: done in
// several pieces (and on which models), or not at all.
func (r *Resilient) recovered(ctx context.Context, sp Speech, err error) {
	log := logx.With(ctx, r.Log)
	if err != nil {
		log.Errorw("tts chunk could not be made, even split up and on the fallback models", "fallbacks", len(r.Fallbacks), "err", err)
		return
	}
	models := map[string]int{}
	for _, p := range sp.Parts {
		models[p.Model]++
	}
	log.Infow("tts chunk made in pieces", "pieces", len(sp.Parts), "pieces_per_model", models)
}

// parts is the call list of a Speech: its own Parts, or the one call it was.
func (s Speech) parts(model, text string) []SpeechPart {
	if len(s.Parts) > 0 {
		return s.Parts
	}
	return []SpeechPart{{Model: model, GenerationID: s.GenerationID, Chars: len([]rune(text)), PCM: s.PCM}}
}

// splittable reports whether cutting the text up could help: the provider failed
// on this input (5xx or no audio). Rate limits, bad keys and cancelled requests
// would fail the same way for every piece, so they are not split.
func splittable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, ErrEmptyAudio) {
		return true
	}
	var se *httpx.StatusError
	return errors.As(err, &se) && se.Code >= 500
}
