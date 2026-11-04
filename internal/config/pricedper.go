package config

// PricedPer is the configured unit a TTS model's price is "per": "token",
// "char" or "second" (models.tts.priced_per for the main model, priced_per on a
// models.tts.fallbacks entry). It is "" when not set, in which case the caller
// guesses it from the price (see pricing.UnitFor).
func PricedPer(c *Config, model string) string {
	if model == "" {
		return ""
	}
	if model == c.GetString("models.tts.model") {
		return c.GetString("models.tts.priced_per")
	}
	var fallbacks []struct {
		Model     string `mapstructure:"model"`
		PricedPer string `mapstructure:"priced_per"`
	}
	if err := c.UnmarshalKey("models.tts.fallbacks", &fallbacks); err != nil {
		return ""
	}
	for _, fb := range fallbacks {
		if fb.Model == model {
			return fb.PricedPer
		}
	}
	return ""
}
