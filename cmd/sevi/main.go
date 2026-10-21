// sevi: URL -> audio, voice Q&A. See design/design.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"sevi/internal/blob"
	"sevi/internal/config"
	"sevi/internal/logx"
	"sevi/internal/reqid"
	"sevi/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	logTest := flag.Bool("log-test", false, "emit one line per log level (including a forced error) and exit")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}

	log, flush, err := logx.New(cfg.Log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logger error:", err)
		os.Exit(1)
	}
	defer flush()

	ctx := reqid.With(context.Background(), reqid.New())
	rlog := logx.With(ctx, log)

	if *logTest {
		rlog.Debugw("log test", "level", "debug")
		rlog.Infow("log test", "level", "info")
		rlog.Warnw("log test", "level", "warn")
		rlog.Errorw("log test: forced error", "operation", "log-test", "err", fmt.Errorf("example failure"))
		return
	}

	if _, err := cfg.ProviderKey(cfg.Models.TTS.Provider); err != nil {
		rlog.Errorw("missing API key", "err", err)
		flush()
		os.Exit(1)
	}
	if cfg.Telegram.Enabled {
		if _, err := config.RequireKey("telegram.bot_token_env", cfg.Telegram.BotTokenEnv); err != nil {
			rlog.Errorw("missing Telegram token", "err", err)
			flush()
			os.Exit(1)
		}
	}
	for field, ok := range cfg.KeyStatus() {
		if !ok {
			rlog.Warnw("API key not set", "field", field)
		}
	}

	if args := flag.Args(); len(args) > 0 {
		switch {
		case args[0] == "tts" && len(args) >= 2:
			// sevi tts [-refresh] <url>
			fs := flag.NewFlagSet("tts", flag.ExitOnError)
			refresh := fs.Bool("refresh", false, "ignore cached text and audio and regenerate")
			fs.Parse(args[1:])
			if fs.NArg() != 1 {
				fmt.Fprintln(os.Stderr, "usage: sevi tts [-refresh] <url>")
				os.Exit(2)
			}
			url := fs.Arg(0)
			st, err := store.Open(ctx, filepath.Join(cfg.App.DataDir, "sevi.db"), rlog)
			if err != nil {
				rlog.Errorw("open store failed", "err", err)
				flush()
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			defer st.Close()
			bl := blob.Disk{Root: filepath.Join(cfg.App.DataDir, "blobs")}
			if err := runTTS(ctx, rlog, cfg, st, bl, url, *refresh); err != nil {
				rlog.Errorw("tts failed", "url", url, "err", err)
				flush()
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		default:
			fmt.Fprintln(os.Stderr, "usage: sevi [-config path] [tts <url>]")
			os.Exit(2)
		}
	}

	rlog.Infow("sevi started",
		"config", *cfgPath,
		"listen", cfg.App.Listen,
		"tts_model", cfg.Models.TTS.Model,
		"llm_model", cfg.Models.LLM.Model,
		"stt_separate", cfg.Models.STT != nil,
		"telegram", cfg.Telegram.Enabled,
		"log_dir", cfg.Log.Dir,
	)
}
