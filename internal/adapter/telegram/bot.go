package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"sevi/internal/httpx"
)

// botAPI is the small slice of the Telegram Bot API we use, over plain HTTP.
type botAPI struct {
	base  string // https://api.telegram.org
	token string
}

type update struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		From      *user  `json:"from"`
		Chat      chat   `json:"chat"`
		Text      string `json:"text"`
	} `json:"message"`
	CallbackQuery *struct {
		ID      string `json:"id"`
		From    user   `json:"from"`
		Data    string `json:"data"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      chat  `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

type user struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type chat struct {
	ID int64 `json:"id"`
}

type button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

type keyboard [][]button

// call posts JSON to a Bot API method and decodes result into out (may be nil).
func (b *botAPI) call(ctx context.Context, method string, params map[string]any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.url(method), bytes.NewReader(body))
	if err != nil {
		return b.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return b.do(ctx, req, method, out)
}

func (b *botAPI) url(method string) string { return b.base + "/bot" + b.token + "/" + method }

func (b *botAPI) do(ctx context.Context, req *http.Request, method string, out any) error {
	raw, _, err := httpx.Do(ctx, req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, b.redact(err))
	}
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram %s: bad response: %w", method, err)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// redact keeps the bot token out of error text: transport errors embed the
// full request URL, which contains the token.
func (b *botAPI) redact(err error) error {
	if err == nil || b.token == "" || !strings.Contains(err.Error(), b.token) {
		return err
	}
	return &redactedError{msg: strings.ReplaceAll(err.Error(), b.token, "<token>"), err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

func (b *botAPI) getUpdates(ctx context.Context, offset, timeoutSec int) ([]update, error) {
	var ups []update
	err := b.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeoutSec,
		"allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

func markup(k keyboard) map[string]any {
	if k == nil {
		return map[string]any{"inline_keyboard": [][]button{}}
	}
	return map[string]any{"inline_keyboard": k}
}

func (b *botAPI) sendMessage(ctx context.Context, chatID int64, text string, k keyboard) (int64, error) {
	p := map[string]any{"chat_id": chatID, "text": text}
	if k != nil {
		p["reply_markup"] = markup(k)
	}
	var m struct {
		MessageID int64 `json:"message_id"`
	}
	err := b.call(ctx, "sendMessage", p, &m)
	return m.MessageID, err
}

// editMessage replaces text; a nil keyboard removes any buttons.
func (b *botAPI) editMessage(ctx context.Context, chatID, msgID int64, text string, k keyboard) error {
	err := b.call(ctx, "editMessageText", map[string]any{
		"chat_id": chatID, "message_id": msgID, "text": text, "reply_markup": markup(k),
	}, nil)
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

func (b *botAPI) editCaption(ctx context.Context, chatID, msgID int64, caption string) error {
	err := b.call(ctx, "editMessageCaption", map[string]any{"chat_id": chatID, "message_id": msgID, "caption": caption}, nil)
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil
	}
	return err
}

func (b *botAPI) deleteMessage(ctx context.Context, chatID, msgID int64) error {
	return b.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": msgID}, nil)
}

func (b *botAPI) answerCallback(ctx context.Context, id, text string) error {
	return b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

type audioMeta struct {
	Filename, Title, Performer, Caption string
	DurationSec                         int
}

// sendAudio uploads an MP3 as an audio message (seek bar, shows in the music
// player), which suits long files better than a voice note.
func (b *botAPI) sendAudio(ctx context.Context, chatID int64, mp3 []byte, m audioMeta) (int64, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range map[string]string{
		"chat_id": strconv.FormatInt(chatID, 10), "title": m.Title, "performer": m.Performer,
		"caption": m.Caption, "duration": strconv.Itoa(m.DurationSec),
	} {
		if v != "" {
			if err := w.WriteField(k, v); err != nil {
				return 0, err
			}
		}
	}
	part, err := w.CreateFormFile("audio", m.Filename)
	if err != nil {
		return 0, err
	}
	if _, err := part.Write(mp3); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	req, err := http.NewRequest("POST", b.url("sendAudio"), &buf)
	if err != nil {
		return 0, b.redact(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	if err := b.do(ctx, req, "sendAudio", &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

var errAudioTooLarge = errors.New("audio is larger than Telegram's 50 MB bot upload limit")

const maxUploadBytes = 49 << 20
