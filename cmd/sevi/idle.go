package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"sevi/internal/idle"
)

// newIdle builds the idle manager from config (idle.*, all optional with
// defaults), or returns nil in dev mode (app.dev), where the server runs until stopped. The keep-warm
// ping goes to the KEEPALIVE_URL secret, the public gateway URL of /keepalive
// (with its key); without it no pings are sent.
func (a *app) newIdle() *idle.Manager {
	if a.Cfg.GetBool("app.dev") {
		return nil
	}
	c := idle.Config{
		Initial:     a.Minutes("idle.initial_min"),
		BlogWindow:  a.Minutes("idle.blog_min"),
		FirstAnswer: a.Minutes("idle.first_answer_min"),
		Extend:      a.Minutes("idle.extend_min"),
		Cap:         a.Minutes("idle.cap_min"),
		PingEvery:   a.Minutes("idle.keepalive_min"),
	}
	url, secret := a.OptionalSecret("KEEPALIVE_URL"), a.OptionalSecret("TELEGRAM_WEBHOOK_SECRET")
	if url != "" && secret != "" {
		c.Ping = keepWarmPing(url, secret)
	} else {
		a.Log.Infow("no KEEPALIVE_URL secret: the instance relies on Cloud Run's own idle timeout between pings")
	}
	return idle.New(c, a.Log)
}

// keepWarmPing POSTs to the gateway URL, which forwards to /keepalive.
func keepWarmPing(url, secret string) func(context.Context) error {
	client := &http.Client{Timeout: 20 * time.Second}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("keepalive answered %s", resp.Status)
		}
		return nil
	}
}
