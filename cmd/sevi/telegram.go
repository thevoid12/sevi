package main

import (
	"context"
	"fmt"
	"os"

	"sevi/internal/adapter/telegram"
)

// telegramCmd runs `sevi telegram <set-webhook [url] | webhook-info | delete-webhook>`.
// It needs only secrets (no database), and the logic lives in the adapter.
func (a *app) telegramCmd(ctx context.Context, args []string) int {
	token, err := a.Secret("telegram", "TELEGRAM_BOT_TOKEN")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	admin := telegram.NewAdmin(token, "")
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	show := func() int {
		info, err := admin.Info(ctx)
		if err != nil {
			return fail(err)
		}
		fmt.Println(info)
		return 0
	}
	switch {
	case len(args) >= 1 && args[0] == "set-webhook" && len(args) <= 2:
		secret, err := a.Secret("telegram", "TELEGRAM_WEBHOOK_SECRET")
		if err != nil {
			return fail(err)
		}
		var url string
		if len(args) == 2 {
			url = args[1]
		} else if url, err = a.Secret("telegram", "TELEGRAM_WEBHOOK_URL"); err != nil {
			return fail(fmt.Errorf("%w (or pass the URL: sevi telegram set-webhook <url>)", err))
		}
		if err := admin.SetWebhook(ctx, url, secret); err != nil {
			return fail(err)
		}
		fmt.Println("webhook set")
		return show()
	case len(args) == 1 && args[0] == "webhook-info":
		return show()
	case len(args) == 1 && args[0] == "delete-webhook":
		if err := admin.DeleteWebhook(ctx); err != nil {
			return fail(err)
		}
		fmt.Println("webhook deleted")
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: sevi telegram set-webhook [url] | webhook-info | delete-webhook")
	return 2
}
