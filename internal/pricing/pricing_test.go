package pricing

import (
	"testing"

	"sevi/internal/provider"
)

func TestUnitFor(t *testing.T) {
	gemini := provider.Price{Prompt: 5e-7, Completion: 6e-6}
	voxtral := provider.Price{Prompt: 1.3e-5}
	seed := provider.Price{Completion: 0.0025}
	for _, tc := range []struct {
		name, configured string
		p                provider.Price
		want             Unit
	}{
		{"both prices: token", "", gemini, UnitToken},
		{"prompt only: char", "", voxtral, UnitChar},
		{"completion only: second", "", seed, UnitSecond},
		{"config wins over the price shape", "token", voxtral, UnitToken},
		{"config is case and space tolerant", " Char ", gemini, UnitChar},
		{"unknown config is ignored", "banana", voxtral, UnitChar},
	} {
		if got := UnitFor(tc.configured, tc.p); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestTTSCostByUnit(t *testing.T) {
	const chars, seconds, tps = 3000, 180.0, 25.0
	cost := func(p provider.Price, u Unit) float64 {
		in, out := TTSUnits(u, chars, seconds, tps)
		return TTSCost(p, in, out)
	}
	// Gemini lite: 667 input tokens + 4500 audio tokens.
	if got := cost(provider.Price{Prompt: 5e-7, Completion: 6e-6}, UnitToken); got < 0.0273 || got > 0.0274 {
		t.Errorf("per token: %v", got)
	}
	// Voxtral: $13 per million characters.
	if got := cost(provider.Price{Prompt: 1.3e-5}, UnitChar); got < 0.0389 || got > 0.0391 {
		t.Errorf("per char: %v, want 0.039", got)
	}
	// Seed: $0.0025 per second of audio.
	if got := cost(provider.Price{Completion: 0.0025}, UnitSecond); got < 0.449 || got > 0.451 {
		t.Errorf("per second: %v, want 0.45", got)
	}
	// Counting a per-character model by tokens would undercount by 4.5x: that is the bug this fixes.
	in, out := TTSUnits(UnitToken, chars, seconds, tps)
	if wrong := TTSCost(provider.Price{Prompt: 1.3e-5}, in, out); wrong > 0.0095 {
		t.Errorf("sanity: token counting of a per-char price = %v", wrong)
	}
}
