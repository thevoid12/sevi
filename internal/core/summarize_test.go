package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"sevi/internal/config"
	"sevi/internal/provider"
)

func isSummaryCall(req provider.ChatRequest) bool { return strings.Contains(req.System, "condense") }

// summarising env: summarise once more than 700 characters are unsummarised,
// folding the oldest until at most 350 remain in full. Answers are long enough to add up.
func newSummaryEnv(t *testing.T) *env {
	e := newEnv(t, 100, 0.014)
	e.core.Cfg.Chat = config.Conversation{SummarizeOverChars: 700}
	long := strings.Repeat("The answer goes on for a while. ", 8) // ~260 chars
	e.llm.replies = []string{replyJSON("", "text", long)}
	e.llm.hook = func(req provider.ChatRequest) (string, error, bool) {
		if isSummaryCall(req) {
			return "NOTES: the user asked about points one to three.", nil, true
		}
		return "", nil, false
	}
	return e
}

func (e *env) summaryCalls() []provider.ChatRequest {
	e.llm.mu.Lock()
	defer e.llm.mu.Unlock()
	var out []provider.ChatRequest
	for _, r := range e.llm.reqs {
		if isSummaryCall(r) {
			out = append(out, r)
		}
	}
	return out
}

func (e *env) lastQuestionPrompt() string {
	e.llm.mu.Lock()
	defer e.llm.mu.Unlock()
	for i := len(e.llm.reqs) - 1; i >= 0; i-- {
		if !isSummaryCall(e.llm.reqs[i]) {
			var b strings.Builder
			for _, p := range e.llm.reqs[i].Parts {
				b.WriteString(p.Text)
			}
			return b.String()
		}
	}
	return ""
}

func TestALongConversationIsSummarisedAndTheModelGetsSummaryPlusRecentMessages(t *testing.T) {
	e := newSummaryEnv(t)
	ctx := context.Background()
	blog := e.blogWithThread(t, "https://blog.example/a", "1:10")

	var last JobView
	for i := 1; i <= 4; i++ {
		last = e.send(t, ask("1:10", fmt.Sprintf("1:%d", 20+i), fmt.Sprintf("Question number %d about the blog?", i)))
	}
	calls := e.summaryCalls()
	if len(calls) == 0 {
		t.Fatalf("a conversation of this size should have been summarised; sessions: %v", last.SessionID)
	}
	sess, _ := e.st.GetSession(ctx, blog.SessionID)
	if !sess.Summary.Valid || !strings.Contains(sess.Summary.String, "NOTES:") || !sess.SummaryThrough.Valid {
		t.Fatalf("the summary and the message it runs through must be stored: %+v", sess)
	}

	// The next question: summary + recent messages, not the folded ones.
	e.send(t, ask("1:10", "1:30", "Question number 5 about the blog?"))
	prompt := e.lastQuestionPrompt()
	for _, want := range []string{"Summary of the earlier conversation", "NOTES: the user asked", "Recent messages, oldest first (older ones are in the summary above)", "Question number 5", "This is a sentence that will be read aloud"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt is missing %q\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Question number 1 about") {
		t.Errorf("a message already folded into the summary must not be sent in full too")
	}
	// The blog's own text is never summarised away.
	if !strings.Contains(prompt, "A Test Post") {
		t.Error("the blog must still be sent in full")
	}
}

func TestReplyingToAMessageThatWasSummarisedStillShowsThatMessage(t *testing.T) {
	e := newSummaryEnv(t)
	e.blogWithThread(t, "https://blog.example/a", "1:10")
	for i := 1; i <= 4; i++ {
		e.send(t, ask("1:10", fmt.Sprintf("1:%d", 20+i), fmt.Sprintf("Question number %d about the blog?", i)))
	}
	// 1:21 is the first question, now folded into the summary. Reply to it.
	e.send(t, ask("1:21", "1:40", "And what about that first point?"))
	prompt := e.lastQuestionPrompt()
	marker := prompt[strings.Index(prompt, "The user is replying to this earlier message"):]
	if !strings.Contains(marker, "Question number 1 about the blog?") {
		t.Errorf("'that first point' must still point at the first question:\n%s", marker)
	}
}

func TestEachSummaryBuildsOnThePreviousOne(t *testing.T) {
	e := newSummaryEnv(t)
	e.blogWithThread(t, "https://blog.example/a", "1:10")
	for i := 1; i <= 8; i++ {
		e.send(t, ask("1:10", fmt.Sprintf("1:%d", 20+i), fmt.Sprintf("Question number %d about the blog?", i)))
	}
	calls := e.summaryCalls()
	if len(calls) < 2 {
		t.Fatalf("expected the conversation to be summarised more than once, got %d", len(calls))
	}
	second := ""
	for _, p := range calls[1].Parts {
		second += p.Text
	}
	if !strings.Contains(second, "Notes so far:") || !strings.Contains(second, "NOTES: the user asked") {
		t.Errorf("a later summary must start from the earlier one:\n%s", second)
	}
}

func TestShortConversationsAreLeftAlone(t *testing.T) {
	e := newSummaryEnv(t)
	e.blogWithThread(t, "https://blog.example/a", "1:10")
	e.send(t, ask("1:10", "1:21", "One short question?"))
	if n := len(e.summaryCalls()); n != 0 {
		t.Errorf("a short conversation was summarised (%d calls)", n)
	}
	// And switching it off in config really switches it off, however long it gets.
	e.core.Cfg.Chat.SummarizeOverChars = 0
	for i := 2; i <= 7; i++ {
		e.send(t, ask("1:10", fmt.Sprintf("1:%d", 20+i), fmt.Sprintf("Question number %d?", i)))
	}
	if n := len(e.summaryCalls()); n != 0 {
		t.Errorf("summarising is off but %d calls were made", n)
	}
}

func TestAFailedSummaryNeverFailsTheQuestion(t *testing.T) {
	e := newSummaryEnv(t)
	e.llm.hook = func(req provider.ChatRequest) (string, error, bool) {
		if isSummaryCall(req) {
			return "", errors.New("summariser down"), true
		}
		return "", nil, false
	}
	e.blogWithThread(t, "https://blog.example/a", "1:10")
	var v JobView
	for i := 1; i <= 5; i++ {
		v = e.send(t, ask("1:10", fmt.Sprintf("1:%d", 20+i), fmt.Sprintf("Question number %d about the blog?", i)))
		if v.Status != Done {
			t.Fatalf("question %d failed because summarising failed: %+v", i, v.Events)
		}
	}
	if sess, _ := e.st.GetSession(context.Background(), v.SessionID); sess.Summary.Valid {
		t.Error("no summary should be stored when summarising failed")
	}
	// Without a summary the model gets the whole history.
	if p := e.lastQuestionPrompt(); !strings.Contains(p, "Question number 1 about") {
		t.Error("with no summary the full history must still be sent")
	}
}

func TestSummarisingIsBilledToTheConversation(t *testing.T) {
	e := newSummaryEnv(t)
	e.blogWithThread(t, "https://blog.example/a", "1:10")
	var v JobView
	for i := 1; i <= 4; i++ {
		v = e.send(t, ask("1:10", fmt.Sprintf("1:%d", 20+i), fmt.Sprintf("Question number %d about the blog?", i)))
	}
	rows, _ := e.st.ListLedger(context.Background(), v.SessionID)
	llm := 0
	for _, r := range rows {
		if r.Role == "llm" && r.Model == "llm-m" {
			llm++
		}
	}
	// 4 answers + at least one summary call.
	if llm < 5 {
		t.Errorf("the summarising call should be in the ledger: %d llm rows for 4 questions", llm)
	}
}

// Only size decides: how much stays in full is "up to half the limit, newest
// first", never a count of messages, and the newest message is never folded.
func TestWhatStaysInFullIsDecidedBySizeAlone(t *testing.T) {
	e := newSummaryEnv(t)
	e.blogWithThread(t, "https://blog.example/a", "1:10")
	var v JobView
	for i := 1; i <= 4; i++ {
		v = e.send(t, ask("1:10", fmt.Sprintf("1:%d", 20+i), fmt.Sprintf("Question number %d about the blog?", i)))
	}
	ctx := context.Background()
	sess, _ := e.st.GetSession(ctx, v.SessionID)
	msgs, _ := e.st.ListMessages(ctx, v.SessionID)
	through := -1
	for i, m := range msgs {
		if m.ID == sess.SummaryThrough.String {
			through = i
		}
	}
	if through < 0 {
		t.Fatalf("no summary boundary stored: %+v", sess)
	}
	kept := 0
	for _, m := range msgs[through+1:] {
		kept += len([]rune(m.Text.String))
	}
	// The messages after the boundary were all asked for before this check; the
	// last turn's own question and answer may have been added since the summary.
	if kept > 350+len("Question number 4 about the blog?")+260+10 {
		t.Errorf("%d characters were left in full, far above half the limit (350)", kept)
	}
	if through == len(msgs)-1 {
		t.Error("the newest message must never be folded into the summary")
	}
}
