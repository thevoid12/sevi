// Package audio encodes and stitches speech with ffmpeg (a hard dependency).
package audio

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// EncodeMP3 pipes PCM through ffmpeg and returns the MP3 bytes. ffmpeg writes
// to a temp file rather than stdout: a seekable output lets it write the
// duration header players need for a correct seek bar.
func EncodeMP3(ctx context.Context, pcm []byte) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH (required for audio encoding): %w", err)
	}
	dir, err := os.MkdirTemp("", "sevi-audio-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "out.mp3")

	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-loglevel", "error",
		"-f", "s16le", "-ar", fmt.Sprint(sampleRate), "-ac", fmt.Sprint(channels), "-i", "-", out)
	cmd.Stdin = bytes.NewReader(pcm)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg encode: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return os.ReadFile(out)
}

// ToMP3 converts audio in any container ffmpeg understands (a Telegram voice
// note is OGG/Opus) to MP3, the format chat models accept.
func ToMP3(ctx context.Context, in []byte) ([]byte, error) {
	return runFFmpeg(ctx, in, nil, []string{"-vn"}, "mp3")
}

// PCMToOpus encodes PCM speech as OGG/Opus, which is what Telegram voice notes
// (with the waveform) require.
func PCMToOpus(ctx context.Context, pcm []byte) ([]byte, error) {
	return runFFmpeg(ctx, pcm,
		[]string{"-f", "s16le", "-ar", fmt.Sprint(sampleRate), "-ac", fmt.Sprint(channels)},
		[]string{"-c:a", "libopus", "-b:a", "32k", "-application", "voip"}, "ogg")
}

// runFFmpeg writes in to a temp file, converts it, and returns the output.
// Files rather than pipes so the output is seekable and gets correct headers.
// inOpts go before -i (describing the input), outOpts after it.
func runFFmpeg(ctx context.Context, in []byte, inOpts, outOpts []string, outExt string) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH (required for audio): %w", err)
	}
	dir, err := os.MkdirTemp("", "sevi-audio-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	inPath, outPath := filepath.Join(dir, "in"), filepath.Join(dir, "out."+outExt)
	if err := os.WriteFile(inPath, in, 0o600); err != nil {
		return nil, err
	}
	args := append([]string{"-y", "-loglevel", "error"}, inOpts...)
	args = append(args, "-i", inPath)
	args = append(args, outOpts...)
	args = append(args, outPath)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg convert to %s: %w: %s", outExt, err, strings.TrimSpace(stderr.String()))
	}
	return os.ReadFile(outPath)
}

// Duration in seconds of a PCM buffer.
func Duration(pcm []byte) float64 {
	return float64(len(pcm)) / float64(sampleRate*channels*2)
}
