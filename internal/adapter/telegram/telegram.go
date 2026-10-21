// Package telegram is the Telegram adapter: it long-polls the Bot API,
// turns messages into Core API calls, and renders Core's events as Telegram
// messages. It holds no business logic and talks to Core only over HTTP.
package telegram

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"sevi/internal/logx"
	"sevi/internal/reqid"
)

const adapterName = "telegram"

type Options struct {
	Token       string
	AllowedIDs  []int64
	CoreURL     string // base URL of the Core API, e.g. http://127.0.0.1:8787
	ShowCost    bool   // append the cost to the audio caption
	TelegramURL string // default https://api.telegram.org (override in tests)
}

type Bot struct {
	api      *botAPI
	core     *seviClient
	allowed  map[int64]bool
	showCost bool
	log      *zap.SugaredLogger

	mu      sync.Mutex
	pending map[int64]string // chat -> session id awaiting a typed yes/no
	wg      sync.WaitGroup
}

func New(o Options, log *zap.SugaredLogger) *Bot {
	if o.TelegramURL == "" {
		o.TelegramURL = "https://api.telegram.org"
	}
	allowed := map[int64]bool{}
	for _, id := range o.AllowedIDs {
		allowed[id] = true
	}
	return &Bot{
		api: &botAPI{base: o.TelegramURL, token: o.Token}, core: &seviClient{base: o.CoreURL},
		allowed: allowed, showCost: o.ShowCost, log: log, pending: map[int64]string{},
	}
}

// Run polls until ctx is cancelled, handling each update in its own goroutine
// so a long job never blocks new messages.
func (b *Bot) Run(ctx context.Context) {
	b.log.Infow("telegram adapter started", "allowed_users", len(b.allowed))
	offset := 0
	backoff := time.Second
	for ctx.Err() == nil {
		ups, err := b.api.getUpdates(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			b.log.Errorw("telegram poll failed", "err", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range ups {
			offset = u.UpdateID + 1
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				b.handle(reqid.With(ctx, reqid.New()), u)
			}()
		}
	}
	b.wg.Wait()
	b.log.Infow("telegram adapter stopped")
}

func (b *Bot) handle(ctx context.Context, u update) {
	log := logx.With(ctx, b.log)
	switch {
	case u.Message != nil && u.Message.From != nil:
		m := u.Message
		if !b.allowed[m.From.ID] {
			log.Warnw("ignored message from a user not on the allowlist", "user_id", m.From.ID, "username", m.From.Username)
			return
		}
		b.onText(ctx, m.Chat.ID, m.From.ID, m.Text)
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil:
		cb := u.CallbackQuery
		if !b.allowed[cb.From.ID] {
			log.Warnw("ignored button press from a user not on the allowlist", "user_id", cb.From.ID, "username", cb.From.Username)
			return
		}
		b.onButton(ctx, cb.ID, cb.Message.Chat.ID, cb.Message.MessageID, cb.From.ID, cb.Data)
	}
}

var reURL = regexp.MustCompile(`https?://[^\s<>"]+`)

func firstURL(text string) string {
	u := reURL.FindString(text)
	return strings.TrimRight(u, ".,;:!?)]}'\"")
}

func (b *Bot) onText(ctx context.Context, chatID, userID int64, text string) {
	log := logx.With(ctx, b.log)
	text = strings.TrimSpace(text)
	if text == "/start" {
		b.say(ctx, chatID, "Send me a link to a blog and I will turn it into audio.")
		return
	}
	if u := firstURL(text); u != "" {
		statusID, err := b.api.sendMessage(ctx, chatID, "Got it. Working on it…", nil)
		if err != nil {
			log.Errorw("could not send status message", "chat", chatID, "err", err)
			return
		}
		job, err := b.core.send(ctx, strconv.FormatInt(userID, 10), fmt.Sprintf("%d:%d", chatID, statusID), "", part{Type: "url", URL: u})
		if err != nil {
			log.Warnw("core rejected the link", "url", u, "err", err)
			b.edit(ctx, chatID, statusID, userMessage(err), nil)
			return
		}
		b.follow(ctx, chatID, statusID, u, job)
		return
	}
	switch strings.ToLower(text) {
	case "yes", "y", "no", "n":
		b.mu.Lock()
		sid, ok := b.pending[chatID]
		delete(b.pending, chatID)
		b.mu.Unlock()
		if ok {
			statusID, err := b.api.sendMessage(ctx, chatID, "Okay…", nil)
			if err != nil {
				log.Errorw("could not send status message", "chat", chatID, "err", err)
				return
			}
			b.confirm(ctx, chatID, statusID, userID, sid, strings.HasPrefix(strings.ToLower(text), "y"))
			return
		}
	}
	b.say(ctx, chatID, "Send me a link to a blog and I will turn it into audio.")
}

// Button data is "c:<session id>:y|n".
func (b *Bot) onButton(ctx context.Context, cbID string, chatID, msgID, userID int64, data string) {
	log := logx.With(ctx, b.log)
	_ = b.api.answerCallback(ctx, cbID, "")
	p := strings.Split(data, ":")
	if len(p) != 3 || p[0] != "c" {
		log.Warnw("unknown button data", "data", data)
		return
	}
	b.mu.Lock()
	delete(b.pending, chatID)
	b.mu.Unlock()
	b.confirm(ctx, chatID, msgID, userID, p[1], p[2] == "y")
}

func (b *Bot) confirm(ctx context.Context, chatID, statusID, userID int64, sessionID string, yes bool) {
	log := logx.With(ctx, b.log)
	decision := "no"
	if yes {
		decision = "yes"
		b.edit(ctx, chatID, statusID, "Okay, starting…", nil)
	}
	job, err := b.core.send(ctx, strconv.FormatInt(userID, 10), "", sessionID, part{Type: "confirm", Decision: decision})
	if err != nil {
		log.Warnw("core rejected the confirmation", "session", sessionID, "err", err)
		b.edit(ctx, chatID, statusID, userMessage(err), nil)
		return
	}
	pageURL := ""
	if s, err := b.core.session(ctx, sessionID); err == nil {
		pageURL = s.URL
	}
	b.follow(ctx, chatID, statusID, pageURL, job)
}

// follow renders a job: progress edits one status message; when the audio is
// ready it is sent and the status message deleted; the cost, which arrives
// later, is added to the audio caption.
func (b *Bot) follow(ctx context.Context, chatID, statusID int64, pageURL string, v jobView) {
	log := logx.With(ctx, b.log).With("session", v.SessionID, "job", v.ID)
	after := 0
	var audioID int64
	var caption func(cost *float64) string
	lastStatus := ""
	deadline := time.Now().Add(20 * time.Minute)

	for {
		if v.Status == "done" && audioID == 0 {
			for _, p := range v.Parts {
				if p.Type != "audio" {
					continue
				}
				id, capFn, err := b.deliver(ctx, chatID, pageURL, v.SessionID, p)
				if err != nil {
					log.Errorw("could not deliver audio", "err", err)
					msg := "I made the audio but couldn't send it to you."
					if err == errAudioTooLarge {
						msg = "The audio is too large to send on Telegram (50 MB limit)."
					}
					b.edit(ctx, chatID, statusID, msg, nil)
					return
				}
				audioID, caption = id, capFn
				_ = b.api.deleteMessage(ctx, chatID, statusID)
			}
		}

		for _, e := range v.Events {
			after = e.Seq
			switch e.Kind {
			case "progress", "notice":
				if audioID == 0 && e.Message != lastStatus {
					lastStatus = e.Message
					b.edit(ctx, chatID, statusID, e.Message, nil)
				}
			case "confirm":
				b.mu.Lock()
				b.pending[chatID] = v.SessionID
				b.mu.Unlock()
				b.edit(ctx, chatID, statusID, e.Message, keyboard{{
					{Text: "Yes", Data: "c:" + v.SessionID + ":y"},
					{Text: "No", Data: "c:" + v.SessionID + ":n"},
				}})
			case "error":
				text := e.Message
				if e.Ref != "" {
					text += " (ref " + e.Ref + ")"
				}
				b.edit(ctx, chatID, statusID, text, nil)
			case "cost":
				if audioID != 0 && b.showCost && e.CostUSD != nil {
					if err := b.api.editCaption(ctx, chatID, audioID, caption(e.CostUSD)); err != nil {
						log.Warnw("could not add cost to caption", "err", err)
					}
				}
			}
		}

		if v.Status == "failed" || v.Status == "needs_confirmation" || (v.Status == "done" && !v.CostPending) {
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			log.Warnw("stopped following job", "status", v.Status)
			return
		}
		next, err := b.core.job(ctx, v.ID, after, 25)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Errorw("could not poll job", "err", err)
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		v = next
	}
}

// deliver downloads the MP3 from Core, sends it, and registers the sent
// message as a thread so a reply to it continues this session. The returned
// function rebuilds the caption, so the cost can be added once it is known.
func (b *Bot) deliver(ctx context.Context, chatID int64, pageURL, sessionID string, p part) (int64, func(*float64) string, error) {
	mp3, err := b.core.audio(ctx, p.MessageID)
	if err != nil {
		return 0, nil, err
	}
	if len(mp3) > maxUploadBytes {
		return 0, nil, errAudioTooLarge
	}
	title := p.Title
	if title == "" {
		title = titleFromURL(pageURL)
	}
	caption := func(cost *float64) string { return buildCaption(title, p.Seconds, cost) }
	id, err := b.api.sendAudio(ctx, chatID, mp3, audioMeta{
		Filename: "audio.mp3", Title: title, Performer: hostOf(pageURL), Caption: caption(nil), DurationSec: p.Seconds,
	})
	if err != nil {
		return 0, nil, err
	}
	if err := b.core.registerThread(ctx, adapterName, fmt.Sprintf("%d:%d", chatID, id), sessionID); err != nil {
		logx.With(ctx, b.log).Warnw("could not register thread", "session", sessionID, "err", err)
	}
	return id, caption, nil
}

// buildCaption is the title and a last line like "9 min · $0.12" (cost added
// once it is known).
func buildCaption(title string, seconds int, cost *float64) string {
	meta := fmt.Sprintf("%d min", (seconds+30)/60)
	if seconds < 90 {
		meta = fmt.Sprintf("%d sec", seconds)
	}
	if cost != nil {
		meta += fmt.Sprintf(" · $%.2f", *cost)
	}
	return title + "\n" + meta
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return strings.TrimPrefix(u.Host, "www.")
	}
	return ""
}

// titleFromURL makes a short readable title: "thisisvoid.in · cheap satisfy".
func titleFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "Audio"
	}
	host := strings.TrimPrefix(u.Host, "www.")
	last := ""
	if segs := strings.Split(strings.Trim(u.Path, "/"), "/"); len(segs) > 0 {
		last = strings.NewReplacer("_", " ", "-", " ").Replace(strings.TrimSuffix(segs[len(segs)-1], ".html"))
	}
	if last == "" {
		return host
	}
	return host + " · " + last
}

func (b *Bot) say(ctx context.Context, chatID int64, text string) {
	if _, err := b.api.sendMessage(ctx, chatID, text, nil); err != nil {
		logx.With(ctx, b.log).Errorw("could not send message", "chat", chatID, "err", err)
	}
}

func (b *Bot) edit(ctx context.Context, chatID, msgID int64, text string, k keyboard) {
	if err := b.api.editMessage(ctx, chatID, msgID, text, k); err != nil {
		logx.With(ctx, b.log).Warnw("could not edit message", "chat", chatID, "err", err)
	}
}
