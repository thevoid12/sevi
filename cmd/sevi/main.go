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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"go.uber.org/zap"

	"sevi/internal/adapter/telegram"
	"sevi/internal/api"
	"sevi/internal/auth"
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

	// `sevi token <adapter>` prints a short-lived API token, for trying the API with curl.
	if len(args) == 2 && args[0] == "token" {
		return printToken(cfg, args[1])
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
			fmt.Fprintln(os.Stderr, "usage: sevi [-config path] [tts <url> | token <adapter>]  (no command: run the server)")
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

// serve runs the HTTP API (and the Telegram adapter, if enabled) until
// interrupted, then drains running jobs.
func serve(ctx context.Context, log *zap.SugaredLogger, cfg *config.Config, cfgPath string, st *store.Store, bl blob.Store) int {
	// Nobody but the configured adapters may call the API: no secrets, no start.
	secret, err := cfg.APISecret()
	if err != nil {
		log.Errorw("the API secret is not set up", "err", err, "hint", "make one with: openssl rand -hex 32")
		return 1
	}
	verifier, err := auth.NewVerifier(secret, cfg.API.Adapters)
	if err != nil {
		log.Errorw("API secrets are not usable", "err", err)
		return 1
	}
	if !auth.IsLoopback(cfg.App.Listen) && !cfg.API.AllowRemoteListen {
		log.Errorw("app.listen is reachable from other machines; refusing to start", "listen", cfg.App.Listen,
			"hint", "use 127.0.0.1, or set api.allow_remote_listen if that is really intended")
		return 1
	}
	c, err := newCore(cfg, st, bl, log)
	if err != nil {
		log.Errorw("could not start core", "err", err)
		return 1
	}
	// Listen first so the adapter's first call to the API cannot race the server.
	ln, err := net.Listen("tcp", cfg.App.Listen)
	if err != nil {
		log.Errorw("could not listen", "addr", cfg.App.Listen, "err", err)
		c.Close()
		return 1
	}
	srv := &http.Server{
		Handler:           api.New(c, st, bl, log, verifier).Handler(),
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
	go func() { errc <- srv.Serve(ln) }()

	var tgDone chan struct{}
	if cfg.Telegram.Enabled {
		token, _ := config.RequireKey("telegram.bot_token_env", cfg.Telegram.BotTokenEnv) // checked at startup
		bot := telegram.New(telegram.Options{
			Token:      token,
			AllowedIDs: cfg.Telegram.AllowedUserIDs,
			CoreURL:    coreURL(ln.Addr()),
			Secret:     secret,
			TokenTTL:   cfg.TokenTTL(),
			ShowCost:   cfg.Cost.ShowInReply,
		}, log)
		tgDone = make(chan struct{})
		go func() { defer close(tgDone); bot.Run(ctx) }()
	}

	select {
	case err := <-errc:
		log.Errorw("http server stopped", "err", err)
		c.Close()
		return 1
	case <-ctx.Done():
	}
	log.Infow("shutting down")
	if tgDone != nil {
		<-tgDone
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Errorw("http shutdown", "err", err)
	}
	c.Close()
	return 0
}

// coreURL is the address adapters use to reach the API we just bound.
func coreURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String()
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// printToken mints a token for one adapter so the API can be tried with curl:
//
//	curl -H "Authorization: Bearer $(sevi token telegram)" localhost:8787/v1/...
func printToken(cfg *config.Config, adapter string) int {
	if !slices.Contains(cfg.API.Adapters, adapter) {
		fmt.Fprintf(os.Stderr, "no adapter %q in api.adapters\n", adapter)
		return 2
	}
	secret, err := cfg.APISecret()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	tok, err := auth.Mint(secret, adapter, cfg.TokenTTL())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(tok)
	return 0
}
