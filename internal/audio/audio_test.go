package audio

import (
	"context"
	"os/exec"
	"testing"
)

func TestConversions(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	pcm := make([]byte, sampleRate*channels*2*2) // 2 seconds of silence

	opus, err := PCMToOpus(ctx, pcm)
	if err != nil {
		t.Fatal(err)
	}
	if string(opus[:4]) != "OggS" {
		t.Errorf("opus output is not an Ogg container: %q", opus[:4])
	}
	// A voice note (OGG/Opus) must convert to MP3 for the model.
	mp3, err := ToMP3(ctx, opus)
	if err != nil {
		t.Fatal(err)
	}
	if len(mp3) < 100 || !(mp3[0] == 0xFF || string(mp3[:3]) == "ID3") {
		t.Errorf("not an mp3: % x", mp3[:4])
	}
}

func TestDuration(t *testing.T) {
	if d := Duration(make([]byte, sampleRate*channels*2*3)); d != 3 {
		t.Errorf("got %v want 3", d)
	}
}
