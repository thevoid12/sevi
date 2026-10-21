package core

import (
	"context"
	"fmt"
)

const defaultCharsPerMinute = 950

// Estimate is a rough price and length for synthesizing a text, shown to the
// user before an expensive job.
type Estimate struct {
	Chars   int
	USD     float64
	Minutes float64
	Source  string // "ledger" (observed average) or "config"
}

// estimate uses the observed average TTS cost per character from the ledger
// and falls back to cost.est_usd_per_1k_chars until there is history.
func (c *Core) estimate(ctx context.Context, chars int) (Estimate, error) {
	cfg := c.Cfg
	perChar, ok, err := c.Store.TTSCostPerChar(ctx, cfg.Models.TTS.Model)
	if err != nil {
		return Estimate{}, err
	}
	src := "ledger"
	if !ok {
		perChar, src = cfg.Cost.EstUSDPer1KChar/1000, "config"
	}
	cpm := cfg.Models.TTS.CharsPerMinute
	if cpm == 0 {
		cpm = defaultCharsPerMinute
	}
	return Estimate{Chars: chars, USD: perChar * float64(chars), Minutes: float64(chars) / float64(cpm), Source: src}, nil
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
