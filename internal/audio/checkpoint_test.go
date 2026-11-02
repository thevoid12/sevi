package audio

import (
	"math"
	"os/exec"
	"testing"
)

func TestCheckpointIsMuchSmallerAndKeepsTheLength(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	// Ten seconds of a 220 Hz tone, 24kHz mono s16le.
	pcm := make([]byte, 10*24000*2)
	for i := 0; i < 10*24000; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*220*float64(i)/24000))
		pcm[2*i], pcm[2*i+1] = byte(v), byte(v>>8)
	}
	small, err := PCMToCheckpoint(t.Context(), pcm)
	if err != nil {
		t.Fatal(err)
	}
	if len(small)*8 > len(pcm) {
		t.Errorf("checkpoint %d bytes for %d raw: want at least 8x smaller", len(small), len(pcm))
	}
	back, err := CheckpointToPCM(t.Context(), small)
	if err != nil {
		t.Fatal(err)
	}
	if d := Duration(back) - 10; d < -0.1 || d > 0.1 {
		t.Errorf("round trip length = %.3fs, want about 10s", Duration(back))
	}
}
