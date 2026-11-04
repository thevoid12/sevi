package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"sevi/internal/audio"
	"sevi/internal/model"
)

// A checkpoint is one object holding every finished range of a half-done blog:
//
//	"SVCK1" | uint32 length of the JSON | JSON | Opus audio of all ranges, in text order
//
// The JSON lists each range of the text and how many milliseconds of audio it
// is. Ranges that touch are stored as one ("1-10 s, 10-20 s, 20-30 s" becomes
// "1-30 s"), so the table stays tiny however many chunks made it.
const checkpointMagic = "SVCK1"

// pcmBytesPerMs is 24 kHz mono 16-bit speech.
const pcmBytesPerMs = 48

// sortedSegs orders segments by position without copying any audio.
func sortedSegs(segs []model.SpeechSeg) []model.SpeechSeg {
	s := append([]model.SpeechSeg(nil), segs...)
	sort.Slice(s, func(i, j int) bool { return s[i].Start < s[j].Start })
	return s
}

// mergeRanges describes the segments as the smallest list of ranges: ranges that
// touch become one ("1-10 s, 10-20 s, 20-30 s" becomes "1-30 s"). Only the table
// is built; the audio is not copied.
func mergeRanges(segs []model.SpeechSeg) []model.RangeMs {
	var out []model.RangeMs
	for _, g := range sortedSegs(segs) {
		ms := len(g.PCM) / pcmBytesPerMs
		if n := len(out); n > 0 && out[n-1].End == g.Start {
			out[n-1].End = g.End
			out[n-1].Ms += ms
			continue
		}
		out = append(out, model.RangeMs{Start: g.Start, End: g.End, Ms: ms})
	}
	return out
}

// prefixSegs is the unbroken run of audio from the start of the text: what can
// be played from the beginning right now. It also reports the runes it covers.
// The audio is returned as it is held, not copied.
func prefixSegs(segs []model.SpeechSeg) (pcm [][]byte, covered int) {
	for _, g := range sortedSegs(segs) {
		if g.Start != covered {
			break
		}
		pcm = append(pcm, g.PCM)
		covered = g.End
	}
	return pcm, covered
}

// coveredRunes is how much of the text the segments cover.
func coveredRunes(segs []model.SpeechSeg) int {
	n := 0
	for _, g := range segs {
		n += g.End - g.Start
	}
	return n
}

// gaps are the ranges of [0, textLen) that the segments do not cover.
func gaps(segs []model.SpeechSeg, textLen int) [][2]int {
	var out [][2]int
	cursor := 0
	for _, g := range sortedSegs(segs) {
		if g.Start > cursor {
			out = append(out, [2]int{cursor, g.Start})
		}
		cursor = max(cursor, g.End)
	}
	if cursor < textLen {
		out = append(out, [2]int{cursor, textLen})
	}
	return out
}

// encodeCheckpoint builds the checkpoint object for the finished ranges.
func encodeCheckpoint(ctx context.Context, textHash string, textLen int, segs []model.SpeechSeg) ([]byte, error) {
	if len(segs) == 0 {
		return nil, errors.New("no audio to keep")
	}
	meta := model.CheckpointMeta{TextHash: textHash, TextLen: textLen, Ranges: mergeRanges(segs)}
	var pcm bytes.Buffer
	for _, g := range sortedSegs(segs) {
		pcm.Write(g.PCM[:len(g.PCM)/pcmBytesPerMs*pcmBytesPerMs]) // whole milliseconds, so the table stays exact
	}
	small, err := audio.PCMToCheckpoint(ctx, pcm.Bytes())
	if err != nil {
		return nil, err
	}
	head, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(checkpointMagic)
	binary.Write(&out, binary.BigEndian, uint32(len(head)))
	out.Write(head)
	out.Write(small)
	return out.Bytes(), nil
}

// decodeCheckpoint reads a checkpoint back into its ranges. It fails when the
// object is not a checkpoint (an older format) or was made for a different text.
func decodeCheckpoint(ctx context.Context, data []byte, textHash string, textLen int) ([]model.SpeechSeg, error) {
	if len(data) < len(checkpointMagic)+4 || string(data[:len(checkpointMagic)]) != checkpointMagic {
		return nil, errors.New("not a checkpoint of this format")
	}
	rest := data[len(checkpointMagic):]
	n := int(binary.BigEndian.Uint32(rest))
	rest = rest[4:]
	if n <= 0 || n > len(rest) {
		return nil, errors.New("checkpoint header is cut off")
	}
	var meta model.CheckpointMeta
	if err := json.Unmarshal(rest[:n], &meta); err != nil {
		return nil, fmt.Errorf("checkpoint header: %w", err)
	}
	if meta.TextHash != textHash || meta.TextLen != textLen {
		return nil, errors.New("the checkpoint was made for a different text")
	}
	pcm, err := audio.CheckpointToPCM(ctx, rest[n:])
	if err != nil {
		return nil, err
	}
	var segs []model.SpeechSeg
	pos := 0
	for i, r := range meta.Ranges {
		if r.Start < 0 || r.End <= r.Start || r.End > textLen {
			return nil, fmt.Errorf("checkpoint range %d is invalid", i)
		}
		end := pos + r.Ms*pcmBytesPerMs
		if i == len(meta.Ranges)-1 || end > len(pcm) { // compression can change the length slightly
			end = len(pcm)
		}
		if end < pos {
			return nil, errors.New("checkpoint audio is shorter than its table")
		}
		segs = append(segs, model.SpeechSeg{Start: r.Start, End: r.End, PCM: pcm[pos:end:end]})
		pos = end
	}
	return segs, nil
}
