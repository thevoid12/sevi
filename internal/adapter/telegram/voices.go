package telegram

import (
	"context"
	"fmt"
	"strings"

	"sevi/internal/logx"
)

// /voices is three taps on one message: gender, then voice, then accent. The
// lists come from Core's /v1/voices, so Telegram holds no catalog of its own and
// a web page can offer exactly the same choices.
//
// Button data (kept under Telegram's 64 bytes, and stateless):
//
//	v:g:<gender>              gender picked, show its voices
//	v:v:<voice>               voice picked, show the accents
//	v:a:<voice>:<accent>      accent picked, save
const voiceButtons = "v"

type voiceChoice struct {
	ID     string `json:"id"`
	Gender string `json:"gender,omitempty"`
	Tone   string `json:"tone,omitempty"`
	Label  string `json:"label,omitempty"`
}

type voiceCatalog struct {
	Genders []string      `json:"genders"`
	Voices  []voiceChoice `json:"voices"`
	Accents []voiceChoice `json:"accents"`
	Current struct {
		Voice  string `json:"voice"`
		Gender string `json:"gender"`
		Accent string `json:"accent"`
	} `json:"current"`
}

func (c *seviClient) voices(ctx context.Context) (voiceCatalog, error) {
	var v voiceCatalog
	err := c.do(ctx, "GET", "/v1/voices", nil, &v)
	return v, err
}

func (c *seviClient) setVoice(ctx context.Context, voice, accent string) error {
	return c.do(ctx, "PUT", "/v1/voice", []byte(fmt.Sprintf(`{"voice":%q,"accent":%q}`, voice, accent)), nil)
}

func (v voiceCatalog) accentLabel(id string) string {
	for _, a := range v.Accents {
		if a.ID == id {
			return a.Label
		}
	}
	return id
}

func (v voiceCatalog) voice(id string) (voiceChoice, bool) {
	for _, x := range v.Voices {
		if x.ID == id {
			return x, true
		}
	}
	return voiceChoice{}, false
}

// nowLine says what is in use.
func (v voiceCatalog) nowLine() string {
	return fmt.Sprintf("Voice now: %s (%s), %s.", v.Current.Voice, v.Current.Gender, v.accentLabel(v.Current.Accent))
}

func (v voiceCatalog) genderKeyboard() keyboard {
	var row []button
	for _, g := range v.Genders {
		row = append(row, button{Text: strings.ToUpper(g[:1]) + g[1:], Data: voiceButtons + ":g:" + g})
	}
	return keyboard{row}
}

func (v voiceCatalog) voiceKeyboard(gender string) keyboard {
	var k keyboard
	var row []button
	for _, x := range v.Voices {
		if x.Gender != gender {
			continue
		}
		row = append(row, button{Text: x.ID + " · " + x.Tone, Data: voiceButtons + ":v:" + x.ID})
		if len(row) == 2 {
			k, row = append(k, row), nil
		}
	}
	if len(row) > 0 {
		k = append(k, row)
	}
	return k
}

func (v voiceCatalog) accentKeyboard(voice string) keyboard {
	var k keyboard
	for _, a := range v.Accents {
		k = append(k, []button{{Text: a.Label, Data: voiceButtons + ":a:" + voice + ":" + a.ID}})
	}
	return k
}

// voices handles /voices: shows the current choice and the first step.
func (b *Bot) voices(ctx context.Context, m in) {
	v, err := b.core.voices(ctx)
	if err != nil {
		logx.With(ctx, b.log).Warnw("could not load the voices", "err", err)
		b.say(ctx, m.chatID, userMessage(err))
		return
	}
	if _, err := b.api.sendMessage(ctx, m.chatID, v.nowLine()+"\nPick a gender:", v.genderKeyboard(), 0); err != nil {
		logx.With(ctx, b.log).Errorw("could not send the voice picker", "chat", m.chatID, "err", err)
	}
}

// voiceButton handles a tap on the picker; p is the button data split on ":".
func (b *Bot) voiceButton(ctx context.Context, chatID, msgID int64, p []string) {
	log := logx.With(ctx, b.log)
	v, err := b.core.voices(ctx)
	if err != nil {
		log.Warnw("could not load the voices", "err", err)
		b.edit(ctx, chatID, msgID, userMessage(err), nil)
		return
	}
	switch {
	case len(p) == 3 && p[1] == "g":
		b.edit(ctx, chatID, msgID, v.nowLine()+"\nPick a voice ("+p[2]+"):", v.voiceKeyboard(p[2]))
	case len(p) == 3 && p[1] == "v":
		if _, ok := v.voice(p[2]); !ok {
			log.Warnw("unknown voice in button", "voice", p[2])
			return
		}
		b.edit(ctx, chatID, msgID, "Voice: "+p[2]+".\nPick an accent:", v.accentKeyboard(p[2]))
	case len(p) == 4 && p[1] == "a":
		if err := b.core.setVoice(ctx, p[2], p[3]); err != nil {
			log.Warnw("could not save the voice", "voice", p[2], "accent", p[3], "err", err)
			b.edit(ctx, chatID, msgID, userMessage(err), nil)
			return
		}
		b.edit(ctx, chatID, msgID, fmt.Sprintf("Done. Voice is %s, %s. It applies to the next thing I make.", p[2], v.accentLabel(p[3])), nil)
	default:
		log.Warnw("unknown voice button data", "data", strings.Join(p, ":"))
	}
}
