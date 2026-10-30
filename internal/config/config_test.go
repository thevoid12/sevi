package config

import (
	"os"
	"testing"

	"sevi/internal/secrets"
)

// secret_sample.json is the source of truth for secret names: every name the
// example config points at must exist there.
func TestSampleCoversExampleConfig(t *testing.T) {
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
	names := []string{cfg.GetString("api.secret_env"), cfg.GetString("telegram.bot_token_env")}
	for name := range cfg.GetStringMap("providers") {
		names = append(names, cfg.GetString("providers."+name+".key_env"))
	}
	for _, n := range names {
		if !secrets.Has(s, n) {
			t.Errorf("secret_sample.json is missing %s", n)
		}
	}
}
