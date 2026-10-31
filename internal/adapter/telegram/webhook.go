package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

// Admin registers and inspects the bot's webhook. It is part of the adapter, so
// the program only needs to call it; nothing here touches Core.
type Admin struct{ api *botAPI }

// NewAdmin makes an Admin for a bot token. base is the Bot API root; empty means
// https://api.telegram.org.
func NewAdmin(token, base string) *Admin {
	if base == "" {
		base = "https://api.telegram.org"
	}
	return &Admin{api: &botAPI{base: base, token: token}}
}

// Telegram only accepts secret tokens of 1-256 characters from this set.
var reSecretToken = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// SetWebhook points Telegram at webhookURL (HTTPS only) and tells it to send the
// secret in the X-Telegram-Bot-Api-Secret-Token header of every request. Only the
// update kinds the adapter handles are requested. Updates queued while no webhook
// was set are kept.
func (a *Admin) SetWebhook(ctx context.Context, webhookURL, secret string) error {
	u, err := url.Parse(webhookURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("the webhook URL must be a full https:// URL")
	}
	if !reSecretToken.MatchString(secret) {
		return errors.New("the webhook secret must be 1-256 characters of A-Z a-z 0-9 _ - (make one with: openssl rand -hex 32)")
	}
	return a.api.call(ctx, "setWebhook", map[string]any{
		"url":             webhookURL,
		"secret_token":    secret,
		"allowed_updates": []string{"message", "callback_query"},
		"max_connections": 4, // one Cloud Run instance does the work
	}, nil)
}

// DeleteWebhook removes the webhook. Pending updates are kept.
func (a *Admin) DeleteWebhook(ctx context.Context) error {
	return a.api.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// WebhookInfo is what Telegram reports about the current webhook. URL has its
// query string removed: the gateway API key travels there and must not be shown.
type WebhookInfo struct {
	URL              string `json:"url"`
	PendingUpdates   int    `json:"pending_update_count"`
	LastErrorDate    int64  `json:"last_error_date"`
	LastErrorMessage string `json:"last_error_message"`
	MaxConnections   int    `json:"max_connections"`
}

func (a *Admin) Info(ctx context.Context) (WebhookInfo, error) {
	var w WebhookInfo
	if err := a.api.call(ctx, "getWebhookInfo", map[string]any{}, &w); err != nil {
		return w, err
	}
	if u, err := url.Parse(w.URL); err == nil && u.RawQuery != "" {
		u.RawQuery = ""
		w.URL = u.String() + "?…"
	}
	return w, nil
}

func (w WebhookInfo) String() string {
	if w.URL == "" {
		return "no webhook is set"
	}
	s := fmt.Sprintf("webhook %s\npending updates: %d, max connections: %d", w.URL, w.PendingUpdates, w.MaxConnections)
	if w.LastErrorMessage != "" {
		s += fmt.Sprintf("\nlast error: %s", w.LastErrorMessage)
	}
	return s
}
