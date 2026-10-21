// Package audio encodes and stitches speech with ffmpeg (a hard dependency).
package audio

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// PCM format returned by Gemini TTS: 24kHz mono s16le.
const (
	sampleRate = 24000
	channels   = 1
)

// Stitch joins PCM chunks into one stream. Chunks share one raw format, so
// joining is concatenation; the single encode afterwards makes one seamless file.
func Stitch(chunks [][]byte) []byte {
	var n int
	for _, c := range chunks {
		n += len(c)
	}
	out := make([]byte, 0, n)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

// EncodeMP3 pipes PCM through ffmpeg into an MP3 file at out.
func EncodeMP3(ctx context.Context, pcm []byte, out string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not found in PATH (required for audio encoding): %w", err)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-loglevel", "error",
		"-f", "s16le", "-ar", fmt.Sprint(sampleRate), "-ac", fmt.Sprint(channels), "-i", "-", out)
	cmd.Stdin = bytes.NewReader(pcm)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg encode to %s: %w: %s", out, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Duration in seconds of a PCM buffer.
func Duration(pcm []byte) float64 {
	return float64(len(pcm)) / float64(sampleRate*channels*2)
}
