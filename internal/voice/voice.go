// Package voice is the catalog of voices and accents a person can choose from,
// and the one chosen setting. It is data only: the API hands the catalog to any
// front end (Telegram buttons today, a web page later), and the speech code
// reads the chosen setting.
package voice

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrUnknown means a voice or accent is not in the catalog.
var ErrUnknown = errors.New("not in the catalog")

const (
	Male   = "male"
	Female = "female"
)

// Voice is one prebuilt Gemini voice. Tone is Google's own word for it.
type Voice struct {
	ID     string `json:"id"`
	Gender string `json:"gender"`
	Tone   string `json:"tone"`
}

// Accent is a way of speaking. Gemini has no accent setting, so it is asked for
// in words sent as the request's "instructions" (never in the text, which is read
// aloud as written). An empty Say is the voice's own accent.
type Accent struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Say   string `json:"-"`
}

// DefaultAccent is the voice as it is, with nothing asked of it (US).
const DefaultAccent = "default"

// Voices are Gemini's prebuilt voices. Gemini has no call that lists them.
var Voices = []Voice{
	{"Zephyr", Female, "Bright"}, {"Kore", Female, "Firm"}, {"Leda", Female, "Youthful"},
	{"Aoede", Female, "Breezy"}, {"Callirrhoe", Female, "Easy-going"}, {"Autonoe", Female, "Bright"},
	{"Despina", Female, "Smooth"}, {"Erinome", Female, "Clear"}, {"Laomedeia", Female, "Upbeat"},
	{"Achernar", Female, "Soft"}, {"Gacrux", Female, "Mature"}, {"Pulcherrima", Female, "Forward"},
	{"Vindemiatrix", Female, "Gentle"}, {"Sulafat", Female, "Warm"},

	{"Puck", Male, "Upbeat"}, {"Charon", Male, "Informative"}, {"Fenrir", Male, "Excitable"},
	{"Orus", Male, "Firm"}, {"Enceladus", Male, "Breathy"}, {"Iapetus", Male, "Clear"},
	{"Umbriel", Male, "Easy-going"}, {"Algieba", Male, "Smooth"}, {"Algenib", Male, "Gravelly"},
	{"Rasalgethi", Male, "Informative"}, {"Alnilam", Male, "Firm"}, {"Schedar", Male, "Even"},
	{"Achird", Male, "Friendly"}, {"Zubenelgenubi", Male, "Casual"}, {"Sadachbia", Male, "Lively"},
	{"Sadaltager", Male, "Knowledgeable"},
}

var Accents = []Accent{
	{DefaultAccent, "Default (US)", ""},
	{"indian", "Indian English", "Speak in an Indian English accent."},
	{"british", "British English", "Speak in a British English accent."},
	{"australian", "Australian English", "Speak in an Australian English accent."},
	{"irish", "Irish English", "Speak in an Irish English accent."},
}

// Genders are the choices of the first step, in the order they are offered.
var Genders = []string{Male, Female}

// Find returns the voice with that id (case-insensitive).
func Find(id string) (Voice, bool) {
	for _, v := range Voices {
		if strings.EqualFold(v.ID, id) {
			return v, true
		}
	}
	return Voice{}, false
}

// FindAccent returns the accent with that id; "" is the default one.
func FindAccent(id string) (Accent, bool) {
	if id == "" {
		id = DefaultAccent
	}
	for _, a := range Accents {
		if strings.EqualFold(a.ID, id) {
			return a, true
		}
	}
	return Accent{}, false
}

// Setting is the chosen voice and accent.
type Setting struct {
	Voice  string `json:"voice"`
	Gender string `json:"gender"`
	Accent string `json:"accent"`
}

// New checks a choice against the catalog and fills in the gender.
func New(voiceID, accentID string) (Setting, error) {
	v, ok := Find(voiceID)
	if !ok {
		return Setting{}, fmt.Errorf("voice %q: %w", voiceID, ErrUnknown)
	}
	a, ok := FindAccent(accentID)
	if !ok {
		return Setting{}, fmt.Errorf("accent %q: %w", accentID, ErrUnknown)
	}
	return Setting{Voice: v.ID, Gender: v.Gender, Accent: a.ID}, nil
}

// Key names the setting in the audio caches. A plain voice with the default
// accent is just its name, as before accents existed, so older audio is still found.
func (s Setting) Key() string {
	if s.Accent == "" || s.Accent == DefaultAccent {
		return s.Voice
	}
	return s.Voice + "@" + s.Accent
}

// Instructions is the delivery instruction that asks for the accent ("" for none).
func (s Setting) Instructions() string {
	a, _ := FindAccent(s.Accent)
	return a.Say
}

type ctxKey struct{}

// With carries the setting a job was started with down to the speech calls, so
// changing the setting while a job runs does not change its voice half way.
func With(ctx context.Context, s Setting) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// From returns the setting a job was started with.
func From(ctx context.Context) (Setting, bool) {
	s, ok := ctx.Value(ctxKey{}).(Setting)
	return s, ok
}
