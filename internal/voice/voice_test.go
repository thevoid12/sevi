package voice

import (
	"context"
	"testing"
)

func TestCatalogIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range Voices {
		if seen[v.ID] {
			t.Errorf("voice %s is listed twice", v.ID)
		}
		seen[v.ID] = true
		if v.Gender != Male && v.Gender != Female {
			t.Errorf("voice %s has gender %q", v.ID, v.Gender)
		}
	}
	if Accents[0].ID != DefaultAccent || Accents[0].Say != "" {
		t.Error("the first accent must be the default one, with nothing to say")
	}
}

func TestNewChecksTheCatalog(t *testing.T) {
	s, err := New("charon", "")
	if err != nil || s.Voice != "Charon" || s.Gender != Male || s.Accent != DefaultAccent {
		t.Fatalf("got %+v, %v", s, err)
	}
	if _, err := New("nobody", ""); err == nil {
		t.Error("an unknown voice must be refused")
	}
	if _, err := New("Charon", "klingon"); err == nil {
		t.Error("an unknown accent must be refused")
	}
}

func TestKeyKeepsOldCachesValid(t *testing.T) {
	if k := (Setting{Voice: "Kore", Accent: DefaultAccent}).Key(); k != "Kore" {
		t.Errorf("default accent key = %q, want the bare voice", k)
	}
	if k := (Setting{Voice: "Charon", Accent: "indian"}).Key(); k != "Charon@indian" {
		t.Errorf("key = %q", k)
	}
}

func TestContextRoundTrip(t *testing.T) {
	if _, ok := From(context.Background()); ok {
		t.Error("nothing set")
	}
	s := Setting{Voice: "Puck"}
	if got, ok := From(With(context.Background(), s)); !ok || got != s {
		t.Errorf("got %+v, %v", got, ok)
	}
}
