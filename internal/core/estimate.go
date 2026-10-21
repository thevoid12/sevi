package core

import (
	"context"
	"fmt"

	"sevi/internal/pricing"
)

const defaultCharsPerMinute = 950

// Estimate is a rough price and length for synthesizing a text, shown to the
// user before an expensive job.
type Estimate struct {
	Chars   int
	USD     float64
	Minutes float64
	Source  string // "price" (stored model price) or "config" (fallback rate)
}

// estimate prices the speech from the model's stored per-token price and the
// configured speaking rate. If no price can be had (offline and never fetched)
// it falls back to cost.est_usd_per_1k_chars.
func (c *Core) estimate(ctx context.Context, chars int) Estimate {
	cfg := c.Cfg
	cpm := cfg.Models.TTS.CharsPerMinute
	if cpm == 0 {
		cpm = defaultCharsPerMinute
	}
	e := Estimate{Chars: chars, Minutes: float64(chars) / float64(cpm)}
	if p, err := c.Prices.Get(ctx, cfg.Models.TTS.Provider, cfg.Models.TTS.Model); err == nil {
		e.USD, e.Source = pricing.EstimateTTS(p, chars, cpm, cfg.Models.TTS.AudioTokensPerSec), "price"
	} else {
		c.Log.Warnw("no model price for the estimate, using cost.est_usd_per_1k_chars", "err", err)
		e.USD, e.Source = cfg.Cost.EstUSDPer1KChar/1000*float64(chars), "config"
	}
	return e
}

func (e Estimate) String() string {
	return fmt.Sprintf("about %s characters, roughly %d minutes of audio, around $%.2f",
		humanCount(e.Chars), int(e.Minutes+0.5), e.USD)
}

func humanCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
