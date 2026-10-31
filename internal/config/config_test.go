package config

import (
	"os"
	"strings"
	"testing"

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
	names := []string{"DATABASE_URL", "SEVI_JWT_SECRET", "TELEGRAM_BOT_TOKEN"}
	for name := range cfg.GetStringMap("providers") {
		names = append(names, strings.ToUpper(name)+"_API_KEY")
	}
	for _, n := range names {
		if !secrets.Has(s, n) {
			t.Errorf("secret_sample.json is missing %s", n)
		}
	}
}
