package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"sevi/internal/audio"
	"sevi/internal/config"
	"sevi/internal/event"
	"sevi/internal/httpx"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"time"

	"go.uber.org/zap"
)

// blogWithThread shares a blog and registers the audio message under a Telegram-style handle.
func (e *env) blogWithThread(t *testing.T, url, ref string) JobView {
	t.Helper()
	v := e.send(t, link(url))
	if v.Status != Done {
		t.Fatalf("blog job: %s %+v", v.Status, v.Events)
	}
	if err := e.st.PutThread(context.Background(), "tg", ref, v.Parts[0].MessageID); err != nil {
		t.Fatal(err)
	}
	return v
}

func ask(replyTo, ref, text string) Message {
	return Message{User: "me", Adapter: "tg", ThreadRef: ref, ReplyToRef: replyTo, Parts: []Part{{Type: "text", Text: text}}}
}

func TestReplyToBlogAudioAnswersByVoiceWithBlogAndTimeInContext(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	ctx := context.Background()
	blog := e.blogWithThread(t, "https://blog.example/a", "1:10")

	v := e.send(t, ask("1:10", "1:11", "What does the second point mean?"))
	if v.Status != Done || len(v.Parts) != 1 || v.Parts[0].Type != "audio" || v.Parts[0].Mime != "audio/ogg" {
		t.Fatalf("want a done job with one ogg voice part, got %+v", v)
	}
	if v.SessionID != blog.SessionID {
		t.Errorf("a reply to the blog audio must continue the blog's session")
	}
	prompt := e.llm.requestText(0)
	if strings.Contains(prompt, "No blog, article or document has been given") {
		t.Error("a blog session must not claim it has no document")
	}
	for _, want := range []string{"This is a sentence that will be read aloud", "A Test Post", "Conversation so far", "(read the blog aloud as audio)", "The user is replying to this earlier message", "New message, sent at 20", "What does the second point mean?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("model context is missing %q\n%s", want, prompt[len(prompt)-600:])
		}
	}
	if !strings.Contains(e.llm.lastRequest().System, "Be brief.") || !strings.Contains(e.llm.lastRequest().System, `default for this turn is "voice"`) {
		t.Errorf("system prompt should carry the configured style and the default format: %q", e.llm.lastRequest().System)
	}

	// The conversation is stored as a tree: question -> blog audio, answer -> question.
	msgs, _ := e.st.ListMessages(ctx, v.SessionID)
	if len(msgs) != 3 {
		t.Fatalf("want blog audio + question + answer, got %d messages", len(msgs))
	}
	q, a := msgs[1], msgs[2]
	if q.Role != "user" || q.ParentID.String != msgs[0].ID || q.Text.String != "What does the second point mean?" {
		t.Errorf("question not stored as a reply to the blog audio: %+v", q)
	}
	if a.Role != "assistant" || a.ParentID.String != q.ID || a.Format.String != "voice" || a.Text.String != "An answer." || !strings.HasSuffix(a.AudioKey.String, ".ogg") {
		t.Errorf("answer not stored correctly: %+v", a)
	}
	// Both of my message and the bot's can be replied to later.
	if m, err := e.st.MessageForThread(ctx, "tg", "1:11"); err != nil || m.ID != q.ID {
		t.Errorf("the question's own handle must be registered: %v %v", m.ID, err)
	}
}

func TestWholeSessionIsOneThreadWhicheverMessageIsRepliedTo(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.llm.replies = []string{replyJSON("", "voice", "First answer text."), replyJSON("", "voice", "Second answer text.")}
	e.blogWithThread(t, "https://blog.example/a", "1:10")

	e.send(t, ask("1:10", "1:11", "First question?"))
	// Go back and reply to the blog audio again, not to the latest message.
	e.send(t, ask("1:10", "1:12", "Second question?"))

	second := e.llm.requestText(1)
	for _, want := range []string{"First question?", "First answer text.", "Second question?"} {
		if !strings.Contains(second, want) {
			t.Errorf("the second question must still see the whole thread, missing %q", want)
		}
	}
	// The replied-to marker names the blog audio, not the first answer.
	marker := second[strings.Index(second, "replying to this earlier message"):]
	marker = marker[:strings.Index(marker, "\n\nNew message")]
	if strings.Contains(marker, "First answer text.") || !strings.Contains(marker, "read the blog aloud") {
		t.Errorf("replied-to marker should be the blog audio: %q", marker)
	}
}

func TestExplicitTextRequestAnswersAsTextWithoutSpeechCost(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.blogWithThread(t, "https://blog.example/a", "1:10")
	ttsBefore := e.tts.calls.Load()
	e.llm.replies = []string{replyJSON("", "text", "Written answer.")}

	v := e.send(t, ask("1:10", "1:11", "Answer in text please"))
	if len(v.Parts) != 1 || v.Parts[0].Type != "text" || v.Parts[0].Text != "Written answer." {
		t.Fatalf("want a text part, got %+v", v.Parts)
	}
	if e.tts.calls.Load() != ttsBefore {
		t.Error("a text answer must not call TTS")
	}
	rows, _ := e.st.ListLedger(context.Background(), v.SessionID)
	for _, r := range rows[2:] { // after the two blog chunks
		if r.Role == "tts" {
			t.Errorf("a text answer must not write a tts ledger row: %+v", r)
		}
	}
}

func TestVoiceOffInConfigMeansTextByDefault(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.core.Cfg.Reply.Voice = false
	e.llm.replies = []string{replyJSON("", "text", "Plain answer.")}
	v := e.send(t, ask("", "1:1", "hello"))
	if !strings.Contains(e.llm.lastRequest().System, `default for this turn is "text"`) {
		t.Errorf("system prompt should state the default is text: %q", e.llm.lastRequest().System)
	}
	if v.Parts[0].Type != "text" {
		t.Errorf("got %+v", v.Parts)
	}
}

func TestMessageThatIsNotAReplyStartsAFreeChat(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	blog := e.blogWithThread(t, "https://blog.example/a", "1:10")
	v := e.send(t, ask("", "1:20", "Unrelated question"))
	if v.SessionID == blog.SessionID {
		t.Fatal("a message with no reply target must not join the blog's session")
	}
	sess, err := e.st.GetSession(context.Background(), v.SessionID)
	if err != nil || sess.Kind != "chat" {
		t.Errorf("want a chat session, got %+v %v", sess, err)
	}
	prompt := e.llm.requestText(0)
	if strings.Contains(prompt, "This is a sentence that will be read aloud") {
		t.Error("a free chat must not see the blog")
	}
	// The model must be told it has no document, so "this blog" cannot be invented.
	if !strings.Contains(prompt, "No blog, article or document has been given to you") || !strings.Contains(prompt, "Never invent its contents") {
		t.Errorf("a chat with no blog must tell the model so:\n%s", prompt)
	}
}

// Regression: a reply to a message sevi had lost (a blog audio sent before
// threading was tracked) was answered in a blank chat, and the model invented
// the blog's contents. It must be refused with an explanation instead.
func TestReplyToAnUnknownMessageIsRefusedNotAnsweredBlind(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	_, err := e.core.Handle(context.Background(), ask("9:99", "1:21", "summarise this blog"))
	var ue *event.UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "don't remember the message you replied to") {
		t.Fatalf("want a clear refusal, got %v", err)
	}
	if len(e.llm.reqs) != 0 {
		t.Error("the model must not be called when the reply target is unknown")
	}
	if e.tts.calls.Load() != 0 {
		t.Error("nothing should be spoken either")
	}
}

func TestVoiceNoteIsHeardByTheModelAndTranscriptStored(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.blogWithThread(t, "https://blog.example/a", "1:10")
	e.llm.replies = []string{replyJSON("why is that so expensive", "voice", "Because of the audio.")}

	ogg, err := audio.PCMToOpus(context.Background(), make([]byte, 48000))
	if err != nil {
		t.Fatal(err)
	}
	v := e.send(t, Message{Adapter: "tg", ThreadRef: "1:11", ReplyToRef: "1:10",
		Parts: []Part{{Type: "audio", Mime: "audio/ogg", Data: ogg}}})
	if v.Status != Done {
		t.Fatalf("%+v", v)
	}
	req := e.llm.lastRequest()
	var heard bool
	for _, p := range req.Parts {
		heard = heard || (len(p.Audio) > 0 && p.AudioFormat == "mp3")
	}
	if !heard {
		t.Error("the model should receive the voice note as mp3 audio")
	}
	msgs, _ := e.st.ListMessages(context.Background(), v.SessionID)
	q := msgs[1]
	if q.Text.String != "why is that so expensive" || q.Format.String != "voice" || !strings.HasSuffix(q.AudioKey.String, ".ogg") {
		t.Errorf("voice question should keep its transcript and original audio: %+v", q)
	}
	if _, err := e.bl.Get(context.Background(), q.AudioKey.String); err != nil {
		t.Errorf("the user's voice note must be stored: %v", err)
	}
}

func TestSeparateTranscriptionRunsFirst(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	stt := &fakeLLM{replies: []string{"  what about the cost  "}, usage: provider.ChatUsage{PromptTokens: 400, AudioTokens: 380, CompletionTokens: 6}}
	e.core.STT = stt
	e.core.Cfg.Models.STT = &config.STT{Provider: "p", Model: "stt-m"}
	e.blogWithThread(t, "https://blog.example/a", "1:10")
	e.llm.replies = []string{replyJSON("", "voice", "It is cheap.")}

	ogg, _ := audio.PCMToOpus(context.Background(), make([]byte, 48000))
	v := e.send(t, Message{Adapter: "tg", ThreadRef: "1:11", ReplyToRef: "1:10", Parts: []Part{{Type: "audio", Mime: "audio/ogg", Data: ogg}}})
	if v.Status != Done {
		t.Fatalf("%+v", v)
	}
	for _, p := range e.llm.lastRequest().Parts {
		if len(p.Audio) > 0 {
			t.Error("with a separate transcriber the main model gets text, not audio")
		}
	}
	if !strings.Contains(e.llm.requestText(0), "what about the cost") {
		t.Error("the transcript should reach the main model")
	}
	rows, _ := e.st.ListLedger(context.Background(), v.SessionID)
	var roles []string
	for _, r := range rows {
		roles = append(roles, r.Role)
	}
	if !strings.Contains(strings.Join(roles, ","), "stt") || !strings.Contains(strings.Join(roles, ","), "llm") {
		t.Errorf("ledger should have stt and llm rows, got %v", roles)
	}
}

func TestTurnCostIsTokensTimesStoredPrice(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.llm.usage = provider.ChatUsage{PromptTokens: 3000, AudioTokens: 1000, CompletionTokens: 200}
	e.llm.replies = []string{replyJSON("", "text", "Short.")}
	v := e.send(t, ask("", "1:1", "hi"))

	want := pricing.LLMCost(testLLMPrice, e.llm.usage)
	sc, _ := e.st.SessionCost(context.Background(), v.SessionID)
	if sc.Calls != 1 || sc.UnknownCalls != 0 || sc.TotalUsd < want-1e-12 || sc.TotalUsd > want+1e-12 {
		t.Errorf("session cost %+v, want one llm call costing %v", sc, want)
	}
	if v.Parts[0].CostUSD == nil || *v.Parts[0].CostUSD != want {
		t.Errorf("the answer part should carry its cost %v, got %v", want, v.Parts[0].CostUSD)
	}
}

func TestModelFailuresShowASafeMessage(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.llm.err = &httpx.StatusError{Host: "llm.example", Code: 402, Status: "402", Body: "SECRET-DETAIL"}
	v := e.send(t, ask("", "1:1", "hi"))
	if v.Status != Failed {
		t.Fatalf("status %s", v.Status)
	}
	last := v.Events[len(v.Events)-1]
	if last.Kind != event.Error || !strings.Contains(last.Message, "out of credit") || strings.Contains(last.Message, "SECRET") {
		t.Errorf("unsafe or unhelpful error event: %+v", last)
	}

	// A model that ignores the JSON contract still gets an answer delivered.
	e2 := newEnv(t, 100, 0.014)
	e2.llm.replies = []string{"just some prose, no json"}
	v2 := e2.send(t, ask("", "1:1", "hi"))
	if v2.Status != Done || len(v2.Parts) != 1 {
		t.Errorf("fallback for non-JSON output: %+v", v2)
	}
}

func TestBadTurnInput(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	_, err := e.core.Handle(context.Background(), Message{Parts: []Part{{Type: "audio", Mime: "audio/ogg"}}})
	var ue *event.UserError
	if !errors.As(err, &ue) {
		t.Errorf("an empty voice note should be rejected with a user message, got %v", err)
	}
}

// Regression: a burst of upstream 429s used to fail the question with "rate
// limiting me" after ~2 seconds of retrying. It must wait, tell the person it is
// waiting, and then answer.
func TestRateLimitedModelIsRetriedAndThePersonSeesIt(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.llm.rateLim = 2
	e.llm.replies = []string{replyJSON("", "text", "Here you go.")}
	e.core.LLM = &provider.Retrying{LLM: e.llm, Retries: 4, Log: zap.NewNop().Sugar(), Backoff: func(int) time.Duration { return time.Millisecond }}

	v := e.send(t, ask("", "1:1", "hello"))
	if v.Status != Done || v.Parts[0].Text != "Here you go." {
		t.Fatalf("the question should be answered after the rate limit passes: %+v", v)
	}
	var notices int
	for _, ev := range v.Events {
		if ev.Kind == event.Progress && strings.Contains(ev.Message, "busy") && strings.Contains(ev.Message, "Trying again") {
			notices++
		}
	}
	if notices != 2 {
		t.Errorf("want a 'busy, trying again' message for each of the 2 retries, got %d: %+v", notices, v.Events)
	}
	if e.llm.rateLim = 0; len(e.llm.reqs) != 3 {
		t.Errorf("model called %d times, want 3 (two rate limits then success)", len(e.llm.reqs))
	}
	// Only the successful call is billed.
	rows, _ := e.st.ListLedger(context.Background(), v.SessionID)
	if len(rows) != 1 || rows[0].Role != "llm" {
		t.Errorf("a failed attempt must not create a ledger row: %+v", rows)
	}
}

// Regression: after the user once asked for text, a later message with no such
// request was also answered as text (the model treated it as a standing
// preference). The prompt must say the request applies to that turn only.
func TestFormatRequestIsForThatTurnOnly(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.send(t, ask("", "1:1", "hello"))
	sys := e.llm.lastRequest().System
	for _, want := range []string{"NEW message explicitly asks", "does not carry over", "decide from the new message alone"} {
		if !strings.Contains(sys, want) {
			t.Errorf("the system prompt must state that a format request is per turn; missing %q\n%s", want, sys)
		}
	}
}

// Regression found while swapping models: a chat model that cannot hear audio
// failed a voice note with a vague "couldn't get an answer". It must say what is
// wrong and that typing still works.
func TestModelThatCannotHearAudioSaysSo(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.llm.err = &httpx.StatusError{Host: "openrouter.ai", Code: 404, Status: "404", Body: `{"message":"No endpoints found that support input audio"}`}
	ogg, _ := audio.PCMToOpus(context.Background(), make([]byte, 48000))
	v := e.send(t, Message{Parts: []Part{{Type: "audio", Mime: "audio/ogg", Data: ogg}}})
	if v.Status != Failed {
		t.Fatalf("status %s", v.Status)
	}
	msg := v.Events[len(v.Events)-1].Message
	if !strings.Contains(msg, "can't listen to voice notes") || !strings.Contains(msg, "models.stt") || strings.Contains(msg, "endpoints") {
		t.Errorf("unhelpful or leaky message: %q", msg)
	}
}
