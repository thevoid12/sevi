package config

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"sevi/internal/secrets"
)

// secret_sample.json is the single list of secrets: it must hold every name the
// code reads, including the key of each provider in config.example.json.
func TestSampleHoldsEverySecretTheCodeReads(t *testing.T) {
	cfg, err := Load("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../secret_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := secrets.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"DATABASE_URL", "SEVI_JWT_SECRET", "TELEGRAM_BOT_TOKEN", "TELEGRAM_WEBHOOK_SECRET", "TELEGRAM_WEBHOOK_URL", "R2_ENDPOINT", "R2_BUCKET", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY"}
	for name := range cfg.GetStringMap("providers") {
		names = append(names, strings.ToUpper(name)+"_API_KEY")
	}
	for _, n := range names {
		if !secrets.Has(s, n) {
			t.Errorf("secret_sample.json is missing %s", n)
		}
	}
}

func TestPricedPer(t *testing.T) {
	c := viper.New()
	c.Set("models.tts.model", "main/m")
	c.Set("models.tts.priced_per", "token")
	c.Set("models.tts.fallbacks", []map[string]any{
		{"model": "a/char", "priced_per": "char"},
		{"model": "b/unset"},
	})
	for model, want := range map[string]string{"main/m": "token", "a/char": "char", "b/unset": "", "other": "", "": ""} {
		if got := PricedPer(c, model); got != want {
			t.Errorf("PricedPer(%q) = %q, want %q", model, got, want)
		}
	}
}
