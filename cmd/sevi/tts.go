package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"sevi/internal/core"
	"sevi/internal/event"
	"sevi/internal/fetch"
)

// runTTS is the CLI front end to Core: `sevi tts <url>` submits the link, prints
// progress, copies the audio to output/<slug>.mp3 and writes a cost report.
// A long blog asks for confirmation on stdin, like an adapter would.
func (a *app) runTTS(ctx context.Context, pageURL string) error {
	cfg, st, objects := a.Cfg, a.Store, a.Objects
	c, err := a.newCore()
	if err != nil {
		return err
	}
	defer c.Close()

	job, err := c.Handle(ctx, core.Message{User: "cli", Parts: []core.Part{{Type: "url", URL: pageURL}}})
	if err != nil {
		return err
	}
	view, _, err := follow(ctx, job, 0)
	if err != nil {
		return err
	}
	if view.Status == core.NeedsConfirmation {
		fmt.Print("> ")
		var ans string
		fmt.Scanln(&ans)
		if ans != "y" && ans != "yes" {
			ans = "no"
		} else {
			ans = "yes"
		}
		job, err = c.Handle(ctx, core.Message{SessionID: view.SessionID, Parts: []core.Part{{Type: "confirm", Decision: ans}}})
		if err != nil {
			return err
		}
		if view, _, err = follow(ctx, job, 0); err != nil {
			return err
		}
	}
	if view.Status != core.Done {
		return fmt.Errorf("job ended with status %s", view.Status)
	}
	for _, p := range view.Parts {
		if p.Type != "audio" {
			continue
		}
		msg, err := st.GetMessage(ctx, p.MessageID)
		if err != nil {
			return err
		}
		mp3, err := objects.Get(ctx, msg.AudioKey.String)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(cfg.GetString("app.output_dir"), 0o755); err != nil {
			return err
		}
		out := filepath.Join(cfg.GetString("app.output_dir"), fetch.Slug(pageURL)+".mp3")
		if err := os.WriteFile(out, mp3, 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", out)
	}
	return a.writeCostReport(ctx, view.SessionID, pageURL)
}

// follow prints a job's events (those after seq `seen`) as they arrive and
// returns when the job stops running, along with the last seq printed.
func follow(ctx context.Context, job *core.Job, seen int) (core.JobView, int, error) {
	for {
		ch := job.Changed()
		v := job.View(seen)
		for _, e := range v.Events {
			seen = e.Seq
			switch e.Kind {
			case event.Error:
				fmt.Printf("error: %s (ref %s)\n", e.Message, e.Ref)
			default:
				fmt.Println(e.Message)
			}
		}
		if v.Status != core.Running {
			return v, seen, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return v, seen, ctx.Err()
		case <-time.After(5 * time.Minute):
			return v, seen, fmt.Errorf("timed out waiting for the job")
		}
	}
}

func (a *app) writeCostReport(ctx context.Context, sessionID, src string) error {
	cfg, st := a.Cfg, a.Store
	sc, err := st.SessionCost(ctx, sessionID)
	if err != nil {
		return err
	}
	rows, err := st.ListLedger(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(rows) == 0 { // cached or declined: nothing was spent, keep any earlier report
		return nil
	}
	var in, out int64
	tbl := ""
	for i, r := range rows {
		in += r.InputUnits.Int64
		out += r.OutputUnits.Int64
		cost := "?"
		if r.CostUsd.Valid {
			cost = fmt.Sprintf("$%.6f", r.CostUsd.Float64)
		}
		tbl += fmt.Sprintf("| %d | %s | %d | %d | %s |\n", i+1, r.GenerationID.String, r.InputUnits.Int64, r.OutputUnits.Int64, cost)
	}
	note := ""
	if sc.UnknownCalls > 0 {
		note = fmt.Sprintf(" (%d call(s) with unknown cost)", sc.UnknownCalls)
	}
	path := filepath.Join(cfg.GetString("app.output_dir"), fetch.Slug(src)+".cost.md")
	md := fmt.Sprintf(`# Cost: %s

- Source: %s
- Session: %s
- Model: %s (via %s)
- Chunks: %d
- Input tokens: %d
- Output (audio) tokens: %d
- **Total: $%.6f**%s

| # | generation id | in tokens | out tokens | cost |
|---|---|---|---|---|
%s`, filepath.Base(path), src, sessionID, cfg.GetString("models.tts.model"), cfg.GetString("models.tts.provider"), len(rows), in, out, sc.TotalUsd, note, tbl)
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("cost: $%.6f (%d in / %d out tokens)\nwrote %s\n", sc.TotalUsd, in, out, path)
	return nil
}
