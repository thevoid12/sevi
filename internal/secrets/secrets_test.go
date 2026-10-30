package secrets

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestParseAndHelpers(t *testing.T) {
	v, err := Parse([]byte(`{"OPENROUTER_API_KEY":"k1","Empty":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := Get(v, "openrouter_api_key"); !ok || got != "k1" {
		t.Errorf("got %q %v", got, ok)
	}
	if _, ok := Get(v, "EMPTY"); ok {
		t.Error("empty value must count as not set")
	}
	if !Has(v, "empty") || Has(v, "nope") {
		t.Error("Has wrong")
	}
	if _, err := Require(v, "f", "NOPE"); err == nil {
		t.Error("Require should fail on missing")
	}
	for _, bad := range []string{``, `{}`, `[]`, `{"A":`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestLocal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret.json")
	os.WriteFile(p, []byte(`{"A_KEY":"x"}`), 0o600)
	v, err := Local{Path: p}.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(v, "A_KEY"); got != "x" {
		t.Errorf("got %q", got)
	}
	if _, err := (Local{Path: filepath.Join(dir, "nope.json")}).Load(context.Background()); err == nil {
		t.Error("missing file should fail")
	}
}

func TestFromSettings(t *testing.T) {
	s := viper.New()
	s.Set("app.dev", true)
	st, err := FromSettings(s)
	if err != nil || st.Kind() != "local" || st.(Local).Path != "secret.json" {
		t.Fatalf("dev: %v %v", st, err)
	}
	s.Set("app.dev", false)
	if _, err := FromSettings(s); err == nil {
		t.Error("prod without secrets.name should fail")
	}
	s.Set("secrets.name", "projects/p/secrets/s/versions/latest")
	if st, err := FromSettings(s); err != nil || st.Kind() != "gcp" {
		t.Fatalf("prod: %v %v", st, err)
	}
}
