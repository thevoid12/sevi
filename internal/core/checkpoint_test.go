package core

import (
	"bytes"
	"context"
	"os/exec"
	"sevi/internal/model"
	"testing"
)

func seg(start, end int, ms int, fill byte) model.SpeechSeg {
	return model.SpeechSeg{Start: start, End: end, PCM: bytes.Repeat([]byte{fill, 0}, ms*pcmBytesPerMs/2)}
}

func TestMergeRangesJoinsTouchingRanges(t *testing.T) {
	got := mergeRanges([]model.SpeechSeg{seg(20, 30, 100, 3), seg(0, 10, 100, 1), seg(10, 20, 100, 2), seg(50, 55, 40, 9)})
	want := []model.RangeMs{{Start: 0, End: 30, Ms: 300}, {Start: 50, End: 55, Ms: 40}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ranges: %+v, want %+v", got, want)
	}
}

func TestPrefixKeepsTextOrder(t *testing.T) {
	pcm, covered := prefixSegs([]model.SpeechSeg{seg(10, 20, 10, 2), seg(0, 10, 10, 1), seg(20, 30, 10, 3)})
	if covered != 30 || len(pcm) != 3 || pcm[0][0] != 1 || pcm[1][0] != 2 || pcm[2][0] != 3 {
		t.Errorf("audio is not in text order: covered=%d pieces=%d", covered, len(pcm))
	}
}

func TestGapsAndPrefix(t *testing.T) {
	segs := []model.SpeechSeg{seg(0, 10, 10, 1), seg(10, 20, 10, 2), seg(40, 50, 10, 3)}
	if g := gaps(segs, 60); len(g) != 2 || g[0] != [2]int{20, 40} || g[1] != [2]int{50, 60} {
		t.Errorf("gaps: %v", g)
	}
	if g := gaps(segs[:1], 10); len(g) != 0 {
		t.Errorf("a covered text has no gaps: %v", g)
	}
	pcm, covered := prefixSegs(segs)
	if covered != 20 || len(pcm) != 2 { // 0-10 and 10-20, both from the start
		t.Errorf("prefix covers %d in %d pieces", covered, len(pcm))
	}
	if _, covered := prefixSegs([]model.SpeechSeg{seg(5, 10, 10, 1)}); covered != 0 {
		t.Error("audio that does not start at the beginning is not a playable prefix")
	}
	if coveredRunes(segs) != 30 {
		t.Errorf("covered runes: %d", coveredRunes(segs))
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	// Real-looking speech (a tone) so the Opus round trip behaves as it would for speech.
	tone := func(ms int) []byte {
		b := make([]byte, ms*pcmBytesPerMs)
		for i := 0; i+1 < len(b); i += 2 {
			v := int16(8000 * float64(((i/2)%100)-50) / 50)
			b[i], b[i+1] = byte(v), byte(v>>8)
		}
		return b
	}
	segs := []model.SpeechSeg{
		{Start: 0, End: 10, PCM: tone(1500)},
		{Start: 10, End: 20, PCM: tone(1500)},
		{Start: 40, End: 50, PCM: tone(2000)},
	}
	data, err := encodeCheckpoint(ctx, "hash", 60, segs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeCheckpoint(ctx, data, "hash", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Start != 0 || got[0].End != 20 || got[1].Start != 40 || got[1].End != 50 {
		t.Fatalf("ranges: %+v", got)
	}
	for i, want := range []int{3000, 2000} { // milliseconds, within what compression may change
		if d := len(got[i].PCM)/pcmBytesPerMs - want; d < -60 || d > 60 {
			t.Errorf("range %d is %d ms, want about %d", i, len(got[i].PCM)/pcmBytesPerMs, want)
		}
	}
	if len(data) > len(segs[0].PCM)*3/4 {
		t.Errorf("checkpoint is not compressed: %d bytes", len(data))
	}
	for name, tc := range map[string]struct {
		data []byte
		hash string
		n    int
	}{
		"other text": {data, "other", 60},
		"other size": {data, "hash", 61},
		"old format": {[]byte("OggS....."), "hash", 60},
		"empty":      {nil, "hash", 60},
		"cut off":    {data[:len(checkpointMagic)+6], "hash", 60},
	} {
		if _, err := decodeCheckpoint(ctx, tc.data, tc.hash, tc.n); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
