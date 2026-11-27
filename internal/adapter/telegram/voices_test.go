package telegram

import (
	"strings"
	"testing"
)

func catalog() voiceCatalog {
	v := voiceCatalog{
		Genders: []string{"male", "female"},
		Voices: []voiceChoice{
			{ID: "Charon", Gender: "male", Tone: "Informative"},
			{ID: "Puck", Gender: "male", Tone: "Upbeat"},
			{ID: "Orus", Gender: "male", Tone: "Firm"},
			{ID: "Kore", Gender: "female", Tone: "Firm"},
		},
		Accents: []voiceChoice{{ID: "default", Label: "Default (US)"}, {ID: "indian", Label: "Indian English"}},
	}
	v.Current.Voice, v.Current.Gender, v.Current.Accent = "Kore", "female", "default"
	return v
}

func TestVoicePickerSteps(t *testing.T) {
	v := catalog()
	if got := v.nowLine(); got != "Voice now: Kore (female), Default (US)." {
		t.Errorf("now line = %q", got)
	}
	if k := v.genderKeyboard(); len(k) != 1 || len(k[0]) != 2 || k[0][0].Data != "v:g:male" {
		t.Errorf("gender keyboard = %+v", k)
	}
	k := v.voiceKeyboard("male")
	if len(k) != 2 || len(k[0]) != 2 || len(k[1]) != 1 || k[0][0].Text != "Charon · Informative" || k[1][0].Data != "v:v:Orus" {
		t.Errorf("voice keyboard (male only, two per row) = %+v", k)
	}
	if k := v.accentKeyboard("Charon"); len(k) != 2 || k[1][0].Data != "v:a:Charon:indian" {
		t.Errorf("accent keyboard = %+v", k)
	}
}

func TestVoiceButtonDataFitsTelegram(t *testing.T) {
	// Telegram refuses callback data over 64 bytes; the longest real voice and accent must fit.
	if d := voiceButtons + ":a:" + strings.Repeat("x", 13) + ":" + "australian"; len(d) > 64 {
		t.Errorf("button data is %d bytes", len(d))
	}
}
