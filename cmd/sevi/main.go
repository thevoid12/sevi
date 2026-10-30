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

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"sevi/internal/adapter/telegram"
	"sevi/internal/api"
	"sevi/internal/auth"
	"sevi/internal/blob"
	"sevi/internal/config"
	"sevi/internal/logx"
	"sevi/internal/reqid"
	"sevi/internal/secrets"
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
	log, flush, err := logx.New(cfg)
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

	backend, sec, err := secrets.Open(ctx, cfgPath)
	if err != nil {
		rlog.Errorw("load secrets failed", "err", err)
		return 1
	}
	rlog.Infow("secrets loaded", "backend", backend.Kind())
	if _, err := providerKey(cfg, sec, cfg.GetString("models.tts.provider")); err != nil {
		rlog.Errorw("missing API key", "err", err)
		return 1
	}
	if cfg.GetBool("telegram.enabled") {
		if _, err := secrets.Require(sec, "telegram.bot_token_env", cfg.GetString("telegram.bot_token_env")); err != nil {
			rlog.Errorw("missing Telegram token", "err", err)
			return 1
		}
	}

	// `sevi token <adapter>` prints a short-lived API token, for trying the API with curl.
	if len(args) == 2 && args[0] == "token" {
		return printToken(cfg, sec, args[1])
	}

	dbURL, err := secrets.Require(sec, "database.url_env", cfg.GetString("database.url_env"))
	if err != nil {
		rlog.Errorw("missing database URL", "err", err)
		return 1
	}
	st, err := store.Open(ctx, dbURL, rlog)
	if err != nil {
		rlog.Errorw("open store failed", "err", err)
		return 1
	}
	defer st.Close()
	bl := blob.Disk{Root: filepath.Join(cfg.GetString("app.data_dir"), "blobs")}

	if len(args) > 0 {
		if args[0] != "tts" || len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: sevi [-config path] [tts <url> | token <adapter>]  (no command: run the server)")
			return 2
		}
		if err := runTTS(ctx, log, cfg, sec, st, bl, args[1]); err != nil {
			rlog.Errorw("tts failed", "url", args[1], "err", err)
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}
	return serve(ctx, log, cfg, sec, cfgPath, st, bl)
}

// serve runs the HTTP API (and the Telegram adapter, if enabled) until
// interrupted, then drains running jobs.
func serve(ctx context.Context, log *zap.SugaredLogger, cfg *config.Config, sec *viper.Viper, cfgPath string, st *store.Store, bl blob.Store) int {
	// Nobody but the configured adapters may call the API: no secrets, no start.
	secret, err := secrets.Require(sec, "api.secret_env", cfg.GetString("api.secret_env"))
	if err != nil {
		log.Errorw("the API secret is not set up", "err", err, "hint", "make one with: openssl rand -hex 32")
		return 1
	}
	verifier, err := auth.NewVerifier([]byte(secret), cfg.GetStringSlice("api.adapters"))
	if err != nil {
		log.Errorw("API secrets are not usable", "err", err)
		return 1
	}
	if !auth.IsLoopback(cfg.GetString("app.listen")) && !cfg.GetBool("api.allow_remote_listen") {
		log.Errorw("app.listen is reachable from other machines; refusing to start", "listen", cfg.GetString("app.listen"),
			"hint", "use 127.0.0.1, or set api.allow_remote_listen if that is really intended")
		return 1
	}
	c, err := newCore(cfg, sec, st, bl, log)
	if err != nil {
		log.Errorw("could not start core", "err", err)
		return 1
	}
	// Listen first so the adapter's first call to the API cannot race the server.
	ln, err := net.Listen("tcp", cfg.GetString("app.listen"))
	if err != nil {
		log.Errorw("could not listen", "addr", cfg.GetString("app.listen"), "err", err)
		c.Close()
		return 1
	}
	srv := &http.Server{
		Handler:           api.New(c, st, bl, log, verifier).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Infow("sevi started",
		"config", cfgPath,
		"listen", cfg.GetString("app.listen"),
		"tts_model", cfg.GetString("models.tts.model"),
		"llm_model", cfg.GetString("models.llm.model"),
		"stt_separate", cfg.IsSet("models.stt.provider"),
		"telegram", cfg.GetBool("telegram.enabled"),
		"log_dir", cfg.GetString("log.dir"),
	)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	var tgDone chan struct{}
	if cfg.GetBool("telegram.enabled") {
		token, _ := secrets.Require(sec, "telegram.bot_token_env", cfg.GetString("telegram.bot_token_env")) // checked at startup
		bot := telegram.New(telegram.Options{
			Token:      token,
			AllowedIDs: config.AllowedUserIDs(cfg),
			CoreURL:    coreURL(ln.Addr()),
			Secret:     []byte(secret),
			TokenTTL:   config.TokenTTL(cfg),
			ShowCost:   cfg.GetBool("cost.show_in_reply"),
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
func printToken(cfg *config.Config, sec *viper.Viper, adapter string) int {
	if !slices.Contains(cfg.GetStringSlice("api.adapters"), adapter) {
		fmt.Fprintf(os.Stderr, "no adapter %q in api.adapters\n", adapter)
		return 2
	}
	secret, err := secrets.Require(sec, "api.secret_env", cfg.GetString("api.secret_env"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	tok, err := auth.Mint([]byte(secret), adapter, config.TokenTTL(cfg))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(tok)
	return 0
}
