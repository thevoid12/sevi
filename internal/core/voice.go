package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"sevi/internal/event"
	"sevi/internal/logx"
	"sevi/internal/store"
	"sevi/internal/voice"
)

const voiceSettingKey = "voice"

// VoiceSetting is the voice and accent in use: the one saved from the UI, or
// the one in config (models.tts.voice, default accent) when none was ever saved.
func (c *Core) VoiceSetting(ctx context.Context) voice.Setting {
	raw, err := c.Store.GetSetting(ctx, voiceSettingKey)
	if err == nil {
		var s voice.Setting
		if json.Unmarshal([]byte(raw), &s) == nil {
			if s, err = voice.New(s.Voice, s.Accent); err == nil {
				return s
			}
		}
		logx.With(ctx, c.Log).Warnw("saved voice setting is not valid, using the one in config", "value", raw)
	} else if !errors.Is(err, store.ErrNotFound) {
		logx.With(ctx, c.Log).Warnw("could not read the voice setting, using the one in config", "err", err)
	}
	name := c.Cfg.GetString("models.tts.voice")
	if s, err := voice.New(name, ""); err == nil {
		return s
	}
	// A voice that is not in the catalog (a custom one in config) is used as it is.
	return voice.Setting{Voice: name, Accent: voice.DefaultAccent}
}

// SetVoice saves the voice and accent. It applies to jobs started after this.
func (c *Core) SetVoice(ctx context.Context, voiceID, accentID string) (voice.Setting, error) {
	s, err := voice.New(voiceID, accentID)
	if err != nil {
		return voice.Setting{}, event.Wrap("I don't know that voice or accent.", err)
	}
	b, _ := json.Marshal(s)
	if err := c.Store.PutSetting(ctx, voiceSettingKey, string(b)); err != nil {
		return voice.Setting{}, event.Wrap("Something went wrong on my side.", fmt.Errorf("save voice setting: %w", err))
	}
	logx.With(ctx, c.Log).Infow("voice changed", "voice", s.Voice, "accent", s.Accent)
	return s, nil
}

// withVoice reads the setting once and carries it down to the speech calls of
// one job, so a change made while the job runs does not switch voice in the middle.
func (c *Core) withVoice(ctx context.Context) (context.Context, voice.Setting) {
	s := c.VoiceSetting(ctx)
	return voice.With(ctx, s), s
}
