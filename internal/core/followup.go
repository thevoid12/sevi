package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"sevi/internal/audio"
	"sevi/internal/event"
	"sevi/internal/fetch"
	"sevi/internal/httpx"
	"sevi/internal/logx"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/store"
)

const maxVoiceBytes = 25 << 20

// handleTurn takes a question (typed or spoken) that is not a link. Which
// conversation it belongs to is decided here, synchronously, so the adapter
// gets a job back at once: an explicit session, else the session of the message
// it replied to, else (only when it replies to nothing) a new free chat.
//
// A reply to a message sevi does not know is refused rather than answered in a
// blank chat: the person meant a particular blog or conversation, and an answer
// without it would be a guess presented as a reply.
func (c *Core) handleTurn(ctx, jobCtx context.Context, m Message, text string, voice *Part) (*Job, error) {
	log := logx.With(ctx, c.Log)
	internal := func(err error) error { return event.Wrap("Something went wrong on my side.", err) }

	if voice != nil && (len(voice.Data) == 0 || len(voice.Data) > maxVoiceBytes) {
		return nil, event.Wrap("That voice note is empty or too large.", fmt.Errorf("voice note of %d bytes", len(voice.Data)))
	}

	var sessionID, parentID string
	switch {
	case m.SessionID != "":
		if _, err := c.Store.GetSession(ctx, m.SessionID); errors.Is(err, store.ErrNotFound) {
			return nil, event.Wrap("I don't know that conversation.", err)
		} else if err != nil {
			return nil, internal(err)
		}
		sessionID = m.SessionID
	case m.Adapter != "" && m.ReplyToRef != "":
		parent, err := c.Store.MessageForThread(ctx, m.Adapter, m.ReplyToRef)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, internal(err)
		}
		if err != nil {
			log.Warnw("reply to a message sevi does not know, refusing instead of answering without context", "adapter", m.Adapter, "reply_to", m.ReplyToRef)
			return nil, event.Wrap("I don't remember the message you replied to, so I can't tell which blog or conversation you mean. "+
				"It may be from before I could track replies. Send the blog link again to start over, or ask without replying to start a new chat.",
				fmt.Errorf("unknown reply target %s %s", m.Adapter, m.ReplyToRef))
		}
		sessionID, parentID = parent.SessionID, parent.ID
	}
	if sessionID == "" {
		sess, err := c.Store.CreateSession(ctx, "chat", "")
		if err != nil {
			return nil, internal(err)
		}
		sessionID = sess.ID
	}

	format := "text"
	if voice != nil {
		format = "voice"
	}
	userMsg, err := c.Store.AddMessage(ctx, store.NewMessage{SessionID: sessionID, Role: "user", Text: text, ParentID: parentID, Format: format})
	if err != nil {
		return nil, internal(err)
	}
	if voice != nil {
		key := "user/" + userMsg.ID + audioExt(voice.Mime)
		if err := c.Blob.Put(ctx, key, voice.Data); err != nil {
			return nil, internal(err)
		}
		if err := c.Store.SetMessageAudio(ctx, userMsg.ID, key); err != nil {
			return nil, internal(err)
		}
	}
	if m.Adapter != "" && m.ThreadRef != "" {
		if err := c.Store.PutThread(ctx, m.Adapter, m.ThreadRef, userMsg.ID); err != nil {
			return nil, internal(err)
		}
	}
	log.Infow("question", "session", sessionID, "message", userMsg.ID, "reply_to", parentID, "voice", voice != nil, "user", m.User)

	var voiceData []byte
	if voice != nil {
		voiceData = voice.Data
	}
	return c.start(jobCtx, sessionID, func(ctx context.Context, j *Job) error {
		return c.turnJob(ctx, j, sessionID, userMsg, text, voiceData)
	}), nil
}

func audioExt(mime string) string {
	switch {
	case strings.Contains(mime, "ogg"), strings.Contains(mime, "opus"):
		return ".ogg"
	case strings.Contains(mime, "mpeg"), strings.Contains(mime, "mp3"):
		return ".mp3"
	case strings.Contains(mime, "wav"):
		return ".wav"
	case strings.Contains(mime, "mp4"), strings.Contains(mime, "m4a"), strings.Contains(mime, "aac"):
		return ".m4a"
	}
	return ".audio"
}

// reply is the JSON the model returns.
type reply struct {
	Transcript string `json:"transcript"`
	Format     string `json:"format"`
	Answer     string `json:"answer"`
}

var replySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"transcript": map[string]any{"type": "string"},
		"format":     map[string]any{"type": "string", "enum": []string{"voice", "text"}},
		"answer":     map[string]any{"type": "string"},
	},
	"required":             []string{"transcript", "format", "answer"},
	"additionalProperties": false,
}

// turnJob answers one question: build the conversation context, ask the model
// (which also hears the audio and returns its transcript), then deliver the
// answer as speech or text as decided.
func (c *Core) turnJob(ctx context.Context, j *Job, sessionID string, userMsg store.Message, typed string, voiceIn []byte) error {
	log := logx.With(ctx, c.Log).With("session", sessionID, "job", j.ID)
	cfg := c.Cfg
	var turnCost float64

	var audioMP3 []byte
	if voiceIn != nil {
		j.progress("Listening…", 0, 0)
		var err error
		if audioMP3, err = audio.ToMP3(ctx, voiceIn); err != nil {
			return event.Wrap("I couldn't read that voice note.", err)
		}
	} else {
		j.progress("Thinking…", 0, 0)
	}

	// Separate transcription (models.stt set): turn the voice note into text first.
	transcript := typed
	if audioMP3 != nil && c.STT != nil {
		res, err := c.STT.Chat(ctx, provider.ChatRequest{
			System:  "You transcribe speech. Output only the exact words spoken, in the language spoken, with no commentary.",
			Parts:   []provider.ChatPart{{Audio: audioMP3, AudioFormat: "mp3"}},
			OnRetry: retryNotice(j),
		})
		if err != nil {
			return event.Wrap(llmMessage(err), fmt.Errorf("transcribe (model %s): %w", cfg.Models.STT.Model, err))
		}
		cost, err := c.recordLLM(ctx, sessionID, "stt", cfg.Models.STT.Provider, cfg.Models.STT.Model, res, len([]rune(res.Text)))
		if err != nil {
			return event.Wrap("Something went wrong on my side.", err)
		}
		turnCost += cost
		transcript, audioMP3 = strings.TrimSpace(res.Text), nil
	}

	convo, err := c.buildContext(ctx, sessionID, userMsg, transcript, audioMP3 != nil)
	if err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	defaultFormat := "text"
	if cfg.Reply.Voice {
		defaultFormat = "voice"
	}
	parts := []provider.ChatPart{{Text: convo}}
	if audioMP3 != nil {
		parts = append(parts, provider.ChatPart{Audio: audioMP3, AudioFormat: "mp3"})
	}

	j.progress("Thinking…", 0, 0)
	res, err := c.LLM.Chat(ctx, provider.ChatRequest{
		System:     systemPrompt(cfg.Models.LLM.SystemPrompt, defaultFormat),
		Parts:      parts,
		SchemaName: "reply",
		Schema:     replySchema,
		OnRetry:    retryNotice(j),
	})
	if err != nil {
		return event.Wrap(llmMessage(err), fmt.Errorf("chat (model %s): %w", cfg.Models.LLM.Model, err))
	}
	cost, err := c.recordLLM(ctx, sessionID, "llm", cfg.Models.LLM.Provider, cfg.Models.LLM.Model, res, len([]rune(res.Text)))
	if err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	turnCost += cost

	var r reply
	if err := json.Unmarshal([]byte(res.Text), &r); err != nil || strings.TrimSpace(r.Answer) == "" {
		log.Warnw("model did not return the expected JSON, using its text as the answer", "err", err)
		r = reply{Format: defaultFormat, Answer: res.Text}
	}
	if r.Format != "voice" && r.Format != "text" {
		r.Format = defaultFormat
	}
	r.Answer = strings.TrimSpace(r.Answer)
	if t := strings.TrimSpace(r.Transcript); t != "" && transcript == "" {
		transcript = t
	}
	if transcript != "" && typed == "" {
		if err := c.Store.SetMessageText(ctx, userMsg.ID, transcript); err != nil {
			return event.Wrap("Something went wrong on my side.", err)
		}
	}
	log.Infow("answered", "format", r.Format, "default_format", defaultFormat, "answer_chars", len([]rune(r.Answer)),
		"prompt_tokens", res.Usage.PromptTokens, "audio_tokens", res.Usage.AudioTokens, "completion_tokens", res.Usage.CompletionTokens)

	if r.Format == "text" {
		msg, err := c.Store.AddMessage(ctx, store.NewMessage{SessionID: sessionID, Role: "assistant", Text: r.Answer, ParentID: userMsg.ID, Format: "text"})
		if err != nil {
			return event.Wrap("Something went wrong on my side.", err)
		}
		c.finishTurn(j, turnCost, Part{Type: "text", MessageID: msg.ID, Text: r.Answer})
		return nil
	}

	// Voice: speak the answer with the TTS model, as an Ogg/Opus voice note.
	j.progress("Recording the answer…", 0, 0)
	var pcm [][]byte
	chunks := fetch.Chunk(r.Answer, cfg.Models.TTS.ChunkChars)
	for i, ch := range chunks {
		sp, err := c.TTS.Synth(ctx, ch)
		if err != nil {
			return event.Wrap(providerMessage(err, i+1, len(chunks)), fmt.Errorf("answer chunk %d of %d (model %s): %w", i+1, len(chunks), cfg.Models.TTS.Model, err))
		}
		cost, err := c.recordTTS(ctx, sessionID, sp, len([]rune(ch)))
		if err != nil {
			return event.Wrap("Something went wrong on my side.", err)
		}
		turnCost += cost
		pcm = append(pcm, sp.PCM)
	}
	all := audio.Stitch(pcm)
	ogg, err := audio.PCMToOpus(ctx, all)
	if err != nil {
		return event.Wrap("I couldn't encode the answer audio.", err)
	}
	key := "answer/" + userMsg.ID + ".ogg"
	if err := c.Blob.Put(ctx, key, ogg); err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	msg, err := c.Store.AddMessage(ctx, store.NewMessage{SessionID: sessionID, Role: "assistant", Text: r.Answer, AudioKey: key, ParentID: userMsg.ID, Format: "voice"})
	if err != nil {
		return event.Wrap("Something went wrong on my side.", err)
	}
	c.finishTurn(j, turnCost, Part{Type: "audio", MessageID: msg.ID, Seconds: int(audio.Duration(all) + 0.5), Mime: "audio/ogg"})
	return nil
}

// retryNotice shows the person that a model call is being retried, so a rate
// limit looks like waiting and not like being stuck.
func retryNotice(j *Job) func(int, time.Duration, error) {
	return func(attempt int, wait time.Duration, _ error) {
		j.progress(fmt.Sprintf("The AI service is busy. Trying again in %d seconds…", int(wait.Seconds()+0.5)), 0, 0)
	}
}

func (c *Core) finishTurn(j *Job, cost float64, out Part) {
	out.CostUSD = &cost
	j.emit(event.Event{Kind: event.Cost, Message: fmt.Sprintf("Cost $%.4f", cost), CostUSD: &cost})
	j.emit(event.Event{Kind: event.Done, Message: "Answered."})
	j.setStatus(Done, []Part{out})
}

// recordLLM writes the ledger row for a chat call, with its cost computed from
// the tokens the provider reported times the stored price (0 and a NULL cost if
// the price is unknown).
func (c *Core) recordLLM(ctx context.Context, sessionID, role, prov, model string, res provider.ChatResult, chars int) (float64, error) {
	e := store.LedgerEntry{
		SessionID: sessionID, Role: role, Provider: prov, Model: model, GenerationID: res.GenerationID,
		Chars: chars, InputUnits: res.Usage.PromptTokens, OutputUnits: res.Usage.CompletionTokens,
	}
	var cost float64
	if p, err := c.Prices.Get(ctx, prov, model); err != nil {
		logx.With(ctx, c.Log).Errorw("no price for the model, cost left unknown", "model", model, "err", err)
	} else {
		cost = pricing.LLMCost(p, res.Usage)
		e.CostUSD = &cost
	}
	if _, err := c.Store.AddLedger(ctx, e); err != nil {
		return 0, fmt.Errorf("write ledger: %w", err)
	}
	return cost, nil
}

func systemPrompt(style, defaultFormat string) string {
	return style + `

Reply with a JSON object with exactly these fields:
- "transcript": what the user said in the attached audio, word for word, in the language spoken. An empty string if the message was typed or there is no audio.
- "format": "voice" or "text". The default for this turn is "` + defaultFormat + `". Use the other one only if the NEW message explicitly asks for it (for example, asks for the answer in writing, or to be told by voice). Otherwise use the default. A format asked for in an earlier message applies only to the answer to that message: it does not carry over, so decide from the new message alone.
- "answer": your reply. For "voice" write for the ear: short, plain sentences, no markdown, lists or symbols, numbers written the way they are spoken. For "text" use short plain text, no markdown.

The conversation so far is given with the time each message was sent. Messages before the new one are context; answer only the new message. Use only the material you were given; if the answer is not in it, say so instead of guessing.`
}

// buildContext renders what the model needs to know: the blog (for a blog
// session), the whole conversation so far in time order with timestamps, which
// earlier message the user is replying to, and the new message itself.
func (c *Core) buildContext(ctx context.Context, sessionID string, userMsg store.Message, transcript string, hasAudio bool) (string, error) {
	sess, err := c.Store.GetSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	msgs, err := c.Store.ListMessages(ctx, sessionID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if sess.Kind != "blog" {
		b.WriteString("This is a general conversation. No blog, article or document has been given to you. " +
			"If the user refers to \"this blog\" or any text you do not have, say plainly that you do not have it and ask them to reply to the blog's audio or send its link. Never invent its contents.\n\n")
	}
	if sess.Kind == "blog" && sess.Url.Valid {
		title, text := "", ""
		if p, err := c.Store.GetPage(ctx, store.URLHash(sess.Url.String)); err == nil {
			title, text = p.Title.String, p.Text
		}
		fmt.Fprintf(&b, "The conversation is about this blog (%s", sess.Url.String)
		if title != "" {
			fmt.Fprintf(&b, ", titled %q", title)
		}
		b.WriteString("). Its text:\n\n")
		if text == "" {
			b.WriteString("(the text is unavailable; say so if you need it)\n")
		} else {
			b.WriteString(text + "\n")
		}
		b.WriteString("\n")
	}

	line := func(m store.Message) string {
		who, what := "User", strings.TrimSpace(m.Text.String)
		if m.Role == "assistant" {
			who = "Assistant"
			if what == "" {
				what = "(read the blog aloud as audio)"
			}
		}
		if what == "" {
			what = "(audio, not transcribed)"
		}
		return fmt.Sprintf("[%s] %s: %s", stamp(m.CreatedOn), who, what)
	}

	var parent *store.Message
	b.WriteString("Conversation so far, oldest first:\n")
	n := 0
	for i := range msgs {
		m := msgs[i]
		if m.ID == userMsg.ID {
			continue
		}
		n++
		b.WriteString(line(m) + "\n")
		if userMsg.ParentID.Valid && m.ID == userMsg.ParentID.String {
			parent = &msgs[i]
		}
	}
	if n == 0 {
		b.WriteString("(nothing yet)\n")
	}
	if parent != nil {
		b.WriteString("\nThe user is replying to this earlier message:\n" + line(*parent) + "\n")
	}
	fmt.Fprintf(&b, "\nNew message, sent at %s:\n", stamp(userMsg.CreatedOn))
	switch {
	case hasAudio:
		b.WriteString("(spoken, in the attached audio)\n")
	default:
		b.WriteString(transcript + "\n")
	}
	return b.String(), nil
}

func stamp(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04 UTC") }

// llmMessage maps a chat-model failure to text that is safe to show.
func llmMessage(err error) string {
	var se *httpx.StatusError
	switch {
	case errors.Is(err, context.Canceled):
		return "I was stopped before finishing (the server is shutting down)."
	case errors.As(err, &se) && (se.Code == 401 || se.Code == 403):
		return "The AI service rejected my API key."
	case errors.As(err, &se) && se.Code == 402:
		return "The AI service says the account is out of credit."
	case errors.As(err, &se) && se.Code == 404 && strings.Contains(se.Body, "input audio"):
		// The configured chat model cannot hear audio. The fix is in config.json,
		// which the person talking to the bot may not be able to edit.
		return "The AI model I'm set up with can't listen to voice notes. Please type your question, or ask whoever runs me to set models.stt in config.json."
	case errors.As(err, &se) && se.Code == 429:
		return "The AI service is rate limiting me. Try again in a minute."
	case errors.As(err, &se) && se.Code >= 500:
		return "The AI service is having problems. Try again shortly."
	default:
		return "I couldn't get an answer just now."
	}
}
