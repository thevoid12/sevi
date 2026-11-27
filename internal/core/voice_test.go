package core

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"sevi/internal/model"
	"sevi/internal/store/storetest"
	"sevi/internal/voice"
)

func voiceCore(t *testing.T, configured string) *Core {
	t.Helper()
	log := zap.NewNop().Sugar()
	cfg := viper.New()
	cfg.Set("models.tts.voice", configured)
	c := New(Deps{Service: &model.Service{Cfg: cfg, Store: storetest.Open(t, log), Log: log}})
	t.Cleanup(c.Close)
	return c
}

func TestVoiceSettingFallsBackToConfigThenFollowsTheChoice(t *testing.T) {
	c := voiceCore(t, "Kore")
	ctx := context.Background()

	if s := c.VoiceSetting(ctx); s.Voice != "Kore" || s.Gender != voice.Female || s.Accent != voice.DefaultAccent {
		t.Fatalf("before any choice: %+v", s)
	}
	if _, err := c.SetVoice(ctx, "Charon", "indian"); err != nil {
		t.Fatal(err)
	}
	if s := c.VoiceSetting(ctx); s.Voice != "Charon" || s.Gender != voice.Male || s.Accent != "indian" || s.Key() != "Charon@indian" {
		t.Fatalf("after the choice: %+v", s)
	}

	// A job pins what it started with, whatever is chosen later.
	pinned, _ := c.withVoice(ctx)
	c.SetVoice(ctx, "Puck", "")
	if s, _ := voice.From(pinned); s.Voice != "Charon" {
		t.Errorf("pinned voice changed to %q", s.Voice)
	}
}

func TestSetVoiceRefusesWhatIsNotInTheCatalog(t *testing.T) {
	c := voiceCore(t, "Kore")
	for _, tc := range [][2]string{{"nobody", ""}, {"Charon", "klingon"}} {
		if _, err := c.SetVoice(context.Background(), tc[0], tc[1]); !errors.Is(err, voice.ErrUnknown) {
			t.Errorf("%v: want ErrUnknown, got %v", tc, err)
		}
	}
	if s := c.VoiceSetting(context.Background()); s.Voice != "Kore" {
		t.Errorf("a refused choice must change nothing: %+v", s)
	}
}

func TestAConfiguredVoiceOutsideTheCatalogIsUsedAsIs(t *testing.T) {
	c := voiceCore(t, "my-custom-voice")
	if s := c.VoiceSetting(context.Background()); s.Voice != "my-custom-voice" || s.Key() != "my-custom-voice" {
		t.Errorf("got %+v", s)
	}
}
