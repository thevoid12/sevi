// sevi: URL -> audio, voice Q&A. See design/design.md.
//
//	sevi                 run the server (HTTP API on app.listen)
//	sevi tts <url>       one-shot: turn a link into output/<slug>.mp3
//	sevi -log-test       write one log line per level and exit
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"

	"sevi/internal/api"
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
	os.Exit(run(*cfgPath, *logTest, flag.Args()))
}

func run(cfgPath string, logTest bool, args []string) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}
	log, flush, err := logx.New(cfg.Log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logger error:", err)
		return 1
	}
	defer flush()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx := reqid.With(sigCtx, reqid.New())
	rlog := logx.With(ctx, log)

	if logTest {
		rlog.Debugw("log test", "level", "debug")
		rlog.Infow("log test", "level", "info")
		rlog.Warnw("log test", "level", "warn")
		rlog.Errorw("log test: forced error", "operation", "log-test", "err", fmt.Errorf("example failure"))
		return 0
	}

	if _, err := cfg.ProviderKey(cfg.Models.TTS.Provider); err != nil {
		rlog.Errorw("missing API key", "err", err)
		return 1
	}
	if cfg.Telegram.Enabled {
		if _, err := config.RequireKey("telegram.bot_token_env", cfg.Telegram.BotTokenEnv); err != nil {
			rlog.Errorw("missing Telegram token", "err", err)
			return 1
		}
	}
	for field, ok := range cfg.KeyStatus() {
		if !ok {
			rlog.Warnw("API key not set", "field", field)
		}
	}

	st, err := store.Open(ctx, filepath.Join(cfg.App.DataDir, "sevi.db"), rlog)
	if err != nil {
		rlog.Errorw("open store failed", "err", err)
		return 1
	}
	defer st.Close()
	bl := blob.Disk{Root: filepath.Join(cfg.App.DataDir, "blobs")}

	if len(args) > 0 {
		if args[0] != "tts" || len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: sevi [-config path] [tts <url>]  (no command: run the server)")
			return 2
		}
		if err := runTTS(ctx, log, cfg, st, bl, args[1]); err != nil {
			rlog.Errorw("tts failed", "url", args[1], "err", err)
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}
	return serve(ctx, log, cfg, cfgPath, st, bl)
}

// serve runs the HTTP API until interrupted, then drains running jobs.
func serve(ctx context.Context, log *zap.SugaredLogger, cfg *config.Config, cfgPath string, st *store.Store, bl blob.Store) int {
	c, err := newCore(cfg, st, bl, log)
	if err != nil {
		log.Errorw("could not start core", "err", err)
		return 1
	}
	srv := &http.Server{
		Addr:              cfg.App.Listen,
		Handler:           api.New(c, st, bl, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Infow("sevi started",
		"config", cfgPath,
		"listen", cfg.App.Listen,
		"tts_model", cfg.Models.TTS.Model,
		"llm_model", cfg.Models.LLM.Model,
		"stt_separate", cfg.Models.STT != nil,
		"telegram", cfg.Telegram.Enabled,
		"log_dir", cfg.Log.Dir,
	)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		log.Errorw("http server stopped", "err", err)
		c.Close()
		return 1
	case <-ctx.Done():
	}
	log.Infow("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Errorw("http shutdown", "err", err)
	}
	c.Close()
	return 0
}
