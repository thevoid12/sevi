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
	"slices"
	"syscall"
	"time"

	"sevi/internal/adapter/telegram"
	"sevi/internal/api"
	"sevi/internal/auth"
	"sevi/internal/config"
	"sevi/internal/idle"
	"sevi/internal/logx"
	"sevi/internal/model"
	"sevi/internal/objectstore"
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
	a := &app{Service: &model.Service{Cfg: cfg, Sec: sec, Log: log}, cfgPath: cfgPath}
	if _, err := a.ProviderKey(cfg.GetString("models.tts.provider")); err != nil {
		rlog.Errorw("missing API key", "err", err)
		return 1
	}
	if cfg.GetBool("telegram.enabled") {
		for _, name := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_WEBHOOK_SECRET"} {
			if _, err := a.Secret("telegram.enabled", name); err != nil {
				rlog.Errorw("missing Telegram secret", "err", err)
				return 1
			}
		}
	}

	// `sevi token <adapter>` prints a short-lived API token, for trying the API with curl.
	if len(args) == 2 && args[0] == "token" {
		return a.printToken(args[1])
	}

	dbURL, err := a.Secret("database", "DATABASE_URL")
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
	a.Store = st
	objects, err := objectstore.FromSettings(cfg, sec)
	if err != nil {
		rlog.Errorw("object store setup failed", "err", err)
		return 1
	}
	a.Objects = objects
	rlog.Infow("object store ready", "backend", objects.Kind())

	if len(args) > 0 {
		if args[0] != "tts" || len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: sevi [-config path] [tts <url> | token <adapter>]  (no command: run the server)")
			return 2
		}
		if err := a.runTTS(ctx, args[1]); err != nil {
			rlog.Errorw("tts failed", "url", args[1], "err", err)
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}
	return a.serve(ctx)
}

// serve runs the HTTP API (and the Telegram adapter, if enabled) until
// interrupted, then drains running jobs.
func (a *app) serve(ctx context.Context) int {
	cfg, log, st, cfgPath := a.Cfg, a.Log, a.Store, a.cfgPath
	// Nobody but the configured adapters may call the API: no secrets, no start.
	secret, err := a.Secret("api", "SEVI_JWT_SECRET")
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
	ctx, cancel := context.WithCancel(ctx) // cancelled by a signal or when the idle window ends
	defer cancel()
	idleMgr := a.newIdle()
	if idleMgr != nil { // a nil *Manager must not become a non-nil Activity
		a.Activity = idleMgr
	}
	c, err := a.newCore()
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
	// The Core API owns "/"; the Telegram adapter is just another client of it
	// (over HTTP, with its own token) whose webhook is mounted beside it.
	mux := http.NewServeMux()
	mux.Handle("/", api.New(a.Service, c, verifier).Handler())
	var bot *telegram.Bot
	if cfg.GetBool("telegram.enabled") {
		token, _ := a.Secret("telegram.enabled", "TELEGRAM_BOT_TOKEN")              // checked at startup
		webhookSecret, _ := a.Secret("telegram.enabled", "TELEGRAM_WEBHOOK_SECRET") // checked at startup
		ttl := config.TokenTTL(cfg)
		bot, err = telegram.New(telegram.Options{
			Token:         token,
			WebhookSecret: webhookSecret,
			AllowedIDs:    config.AllowedUserIDs(cfg),
			CoreURL:       coreURL(ln.Addr()),
			CoreToken:     func() (string, error) { return auth.Mint([]byte(secret), "telegram", ttl) },
			ShowCost:      cfg.GetBool("cost.show_in_reply"),
		}, log)
		if err != nil {
			log.Errorw("could not set up the telegram adapter", "err", err)
			ln.Close()
			c.Close()
			return 1
		}
		mux.Handle("/telegram", bot.Handler(ctx))
		if idleMgr != nil {
			idleMgr.SetBusy(func() bool { return bot.Active() > 0 })
		}
	}
	var handler http.Handler = mux
	if idleMgr != nil {
		if webhookSecret := a.OptionalSecret("TELEGRAM_WEBHOOK_SECRET"); webhookSecret != "" {
			var dbPing func(context.Context) error
			if cfg.GetBool("idle.keepalive_db") {
				dbPing = st.Ping
			}
			mux.Handle("/keepalive", idle.KeepaliveHandler(webhookSecret, dbPing))
		}
		handler = idleMgr.Wrap(mux)
		go idleMgr.Run(ctx, cancel)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
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
	if bot != nil {
		bot.Wait()
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
func (a *app) printToken(adapter string) int {
	cfg := a.Cfg
	if !slices.Contains(cfg.GetStringSlice("api.adapters"), adapter) {
		fmt.Fprintf(os.Stderr, "no adapter %q in api.adapters\n", adapter)
		return 2
	}
	secret, err := a.Secret("api", "SEVI_JWT_SECRET")
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
