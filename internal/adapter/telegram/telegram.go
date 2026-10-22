// Package telegram is the Telegram adapter: it long-polls the Bot API,
// turns messages into Core API calls, and renders Core's events as Telegram
// messages. It holds no business logic and talks to Core only over HTTP.
//
// Three things can arrive: a link (a blog to turn into audio), a question
// (a voice note or text, usually a reply to an earlier message) and a yes/no
// to a cost question. Threading rides on Telegram's own reply feature: every
// message either side sends is registered with Core under "chat_id:message_id",
// and a reply carries the handle of the message it answers.
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

	"sevi/internal/auth"
	"sevi/internal/logx"
	"sevi/internal/reqid"
)

const adapterName = "telegram"

type Options struct {
	Token       string
	AllowedIDs  []int64
	CoreURL     string // base URL of the Core API, e.g. http://127.0.0.1:8787
	Secret      []byte // this adapter's API secret; every call to Core is signed with it
	TokenTTL    time.Duration
	ShowCost    bool   // append the cost to the blog audio caption
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
	ttl := o.TokenTTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	secret := o.Secret
	return &Bot{
		api: &botAPI{base: o.TelegramURL, token: o.Token},
		core: &seviClient{base: o.CoreURL, token: func() (string, error) {
			return auth.Mint(secret, adapterName, ttl)
		}},
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
		var replyTo int64
		if m.ReplyTo != nil {
			replyTo = m.ReplyTo.MessageID
		}
		b.onMessage(ctx, in{chatID: m.Chat.ID, userID: m.From.ID, msgID: m.MessageID, replyTo: replyTo, text: strings.TrimSpace(m.Text), voice: m.Voice != nil, fileID: voiceID(m.Voice), mime: voiceMime(m.Voice)})
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil:
		cb := u.CallbackQuery
		if !b.allowed[cb.From.ID] {
			log.Warnw("ignored button press from a user not on the allowlist", "user_id", cb.From.ID, "username", cb.From.Username)
			return
		}
		b.onButton(ctx, cb.ID, cb.Message.Chat.ID, cb.Message.MessageID, cb.From.ID, cb.Data)
	}
}

// in is one incoming user message, reduced to what the bot needs.
type in struct {
	chatID, userID, msgID, replyTo int64
	text                           string
	voice                          bool
	fileID, mime                   string
}

func voiceID(v *voiceInfo) string {
	if v == nil {
		return ""
	}
	return v.FileID
}

func voiceMime(v *voiceInfo) string {
	if v == nil || v.MimeType == "" {
		return "audio/ogg"
	}
	return v.MimeType
}

var reURL = regexp.MustCompile(`https?://[^\s<>"]+`)

func firstURL(text string) string {
	u := reURL.FindString(text)
	return strings.TrimRight(u, ".,;:!?)]}'\"")
}

func ref(chatID, msgID int64) string { return fmt.Sprintf("%d:%d", chatID, msgID) }

func (b *Bot) onMessage(ctx context.Context, m in) {
	log := logx.With(ctx, b.log)
	user := strconv.FormatInt(m.userID, 10)

	if m.text == "/start" {
		b.say(ctx, m.chatID, "Send me a link to a blog and I will turn it into audio. Reply to any message with a voice note or text to ask about it.")
		return
	}

	// A link: a new blog.
	if u := firstURL(m.text); u != "" && !m.voice {
		statusID, err := b.api.sendMessage(ctx, m.chatID, "Got it. Working on it…", nil, m.msgID)
		if err != nil {
			log.Errorw("could not send status message", "chat", m.chatID, "err", err)
			return
		}
		job, err := b.core.send(ctx, outgoing{User: user, ThreadRef: ref(m.chatID, m.msgID), Parts: []part{{Type: "url", URL: u}}})
		if err != nil {
			log.Warnw("core rejected the link", "url", u, "err", err)
			b.edit(ctx, m.chatID, statusID, userMessage(err), nil)
			return
		}
		b.follow(ctx, m.chatID, statusID, m.msgID, u, job)
		return
	}

	// A typed yes/no to a pending cost question.
	if !m.voice {
		switch strings.ToLower(m.text) {
		case "yes", "y", "no", "n":
			b.mu.Lock()
			sid, ok := b.pending[m.chatID]
			delete(b.pending, m.chatID)
			b.mu.Unlock()
			if ok {
				statusID, err := b.api.sendMessage(ctx, m.chatID, "Okay…", nil, m.msgID)
				if err != nil {
					log.Errorw("could not send status message", "chat", m.chatID, "err", err)
					return
				}
				b.confirm(ctx, m.chatID, statusID, m.userID, sid, strings.HasPrefix(strings.ToLower(m.text), "y"))
				return
			}
		}
	}

	if m.voice || m.text != "" {
		b.ask(ctx, m, user)
	}
}

// ask handles a question, spoken or typed. If it replies to a message, Core
// finds that message's conversation; otherwise it starts a new chat.
func (b *Bot) ask(ctx context.Context, m in, user string) {
	log := logx.With(ctx, b.log)
	label := "Thinking…"
	if m.voice {
		label = "Listening…"
	}
	statusID, err := b.api.sendMessage(ctx, m.chatID, label, nil, m.msgID)
	if err != nil {
		log.Errorw("could not send status message", "chat", m.chatID, "err", err)
		return
	}
	msg := outgoing{User: user, ThreadRef: ref(m.chatID, m.msgID)}
	if m.replyTo != 0 {
		msg.ReplyToRef = ref(m.chatID, m.replyTo)
	}
	if m.voice {
		data, err := b.api.downloadFile(ctx, m.fileID)
		if err != nil {
			log.Errorw("could not download the voice note", "err", err)
			b.edit(ctx, m.chatID, statusID, "I couldn't download your voice note. Try sending it again.", nil)
			return
		}
		msg.Parts = []part{{Type: "audio", Mime: m.mime, Data: data}}
	} else {
		msg.Parts = []part{{Type: "text", Text: m.text}}
	}
	job, err := b.core.send(ctx, msg)
	if err != nil {
		log.Warnw("core rejected the question", "err", err)
		b.edit(ctx, m.chatID, statusID, userMessage(err), nil)
		return
	}
	b.follow(ctx, m.chatID, statusID, m.msgID, "", job)
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
	job, err := b.core.send(ctx, outgoing{User: strconv.FormatInt(userID, 10), SessionID: sessionID, Parts: []part{{Type: "confirm", Decision: decision}}})
	if err != nil {
		log.Warnw("core rejected the confirmation", "session", sessionID, "err", err)
		b.edit(ctx, chatID, statusID, userMessage(err), nil)
		return
	}
	pageURL := ""
	if s, err := b.core.session(ctx, sessionID); err == nil {
		pageURL = s.URL
	}
	b.follow(ctx, chatID, statusID, 0, pageURL, job)
}

// follow renders a job. Progress edits one status message. When the job is
// done: a blog's audio goes out as an audio file (and the status message is
// deleted); a spoken answer as a voice note (status deleted); a text answer
// replaces the status message. Whatever the bot sends is registered with Core
// so a reply to it continues the conversation. replyTo is the user's message
// to thread the answer under (0 for none).
func (b *Bot) follow(ctx context.Context, chatID, statusID, replyTo int64, pageURL string, v jobView) {
	log := logx.With(ctx, b.log).With("session", v.SessionID, "job", v.ID)
	after := 0
	lastStatus := ""
	deadline := time.Now().Add(20 * time.Minute)

	for {
		for _, e := range v.Events {
			after = e.Seq
			switch e.Kind {
			case "progress", "notice":
				if v.Status != "done" && e.Message != lastStatus {
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
			}
		}

		switch v.Status {
		case "failed", "needs_confirmation":
			return
		case "done":
			b.deliver(ctx, chatID, statusID, replyTo, pageURL, v)
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

// deliver sends the result of a finished job and registers what it sent.
func (b *Bot) deliver(ctx context.Context, chatID, statusID, replyTo int64, pageURL string, v jobView) {
	log := logx.With(ctx, b.log).With("session", v.SessionID, "job", v.ID)
	for _, p := range v.Parts {
		var sentID int64
		var err error
		switch {
		case p.Type == "text":
			text := p.Text
			if c := b.costLine(p.CostUSD); c != "" {
				text += "\n\n" + c
			}
			if r := []rune(text); len(r) > 4096 { // Telegram's message limit
				text = string(r[:4095]) + "…"
			}
			err = b.api.editMessage(ctx, chatID, statusID, text, nil)
			sentID = statusID // the status message became the answer
		case p.Type == "audio" && strings.Contains(p.Mime, "ogg"):
			sentID, err = b.sendVoiceAnswer(ctx, chatID, replyTo, p)
			if err == nil {
				_ = b.api.deleteMessage(ctx, chatID, statusID)
			}
		case p.Type == "audio":
			sentID, err = b.sendBlogAudio(ctx, chatID, replyTo, pageURL, p)
			if err == nil {
				_ = b.api.deleteMessage(ctx, chatID, statusID)
			}
		default:
			continue
		}
		if err != nil {
			log.Errorw("could not deliver the result", "part", p.Type, "err", err)
			msg := "I made the answer but couldn't send it to you."
			if err == errAudioTooLarge {
				msg = "The audio is too large to send on Telegram (50 MB limit)."
			}
			b.edit(ctx, chatID, statusID, msg, nil)
			return
		}
		if err := b.core.registerThread(ctx, adapterName, ref(chatID, sentID), p.MessageID); err != nil {
			log.Warnw("could not register thread", "message", p.MessageID, "err", err)
		}
	}
}

func (b *Bot) sendVoiceAnswer(ctx context.Context, chatID, replyTo int64, p part) (int64, error) {
	ogg, err := b.core.audio(ctx, p.MessageID)
	if err != nil {
		return 0, err
	}
	return b.api.sendVoice(ctx, chatID, ogg, p.Seconds, b.costLine(p.CostUSD), replyTo)
}

// sendBlogAudio sends the blog as an audio file (seek bar, title, length, cost).
func (b *Bot) sendBlogAudio(ctx context.Context, chatID, replyTo int64, pageURL string, p part) (int64, error) {
	mp3, err := b.core.audio(ctx, p.MessageID)
	if err != nil {
		return 0, err
	}
	if len(mp3) > maxUploadBytes {
		return 0, errAudioTooLarge
	}
	title := p.Title
	if title == "" {
		title = titleFromURL(pageURL)
	}
	var cost *float64
	if b.showCost {
		cost = p.CostUSD
	}
	return b.api.sendAudio(ctx, chatID, mp3, audioMeta{
		Filename: "audio.mp3", Title: title, Performer: hostOf(pageURL),
		Caption: buildCaption(title, p.Seconds, cost), DurationSec: p.Seconds, ReplyTo: replyTo,
	})
}

// costLine is what an answer cost ("$0.0027"), or "" when cost display is off
// or the cost is unknown.
func (b *Bot) costLine(cost *float64) string {
	if !b.showCost || cost == nil {
		return ""
	}
	return money(*cost)
}

// money formats a dollar amount; answers cost fractions of a cent, so small
// amounts get four decimals rather than rounding to $0.00.
func money(usd float64) string {
	if usd > 0 && usd < 0.01 {
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// buildCaption is the title and a last line like "9 min · $0.12".
func buildCaption(title string, seconds int, cost *float64) string {
	meta := fmt.Sprintf("%d min", (seconds+30)/60)
	if seconds < 90 {
		meta = fmt.Sprintf("%d sec", seconds)
	}
	if cost != nil {
		meta += " · " + money(*cost)
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
	if _, err := b.api.sendMessage(ctx, chatID, text, nil, 0); err != nil {
		logx.With(ctx, b.log).Errorw("could not send message", "chat", chatID, "err", err)
	}
}

func (b *Bot) edit(ctx context.Context, chatID, msgID int64, text string, k keyboard) {
	if err := b.api.editMessage(ctx, chatID, msgID, text, k); err != nil {
		logx.With(ctx, b.log).Warnw("could not edit message", "chat", chatID, "err", err)
	}
}
