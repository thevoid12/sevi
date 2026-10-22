package core

import (
	"context"
	"strings"
	"unicode/utf8"

	"sevi/internal/logx"
	"sevi/internal/provider"
	"sevi/internal/store"
)

const summarizePrompt = `You condense a conversation between a user and an assistant into notes that let the assistant carry on without the full text. Write the notes in two parts.
Questions, in order: one short line per question the user asked, keeping the user's own wording as closely as brevity allows.
Established: the facts, conclusions, terms and open points from the answers, and any preference the user stated about how to be answered (voice or text, length, language).
When given earlier notes, fold the new messages into them: keep every earlier question in its place in the list, add the new ones after, and merge facts that repeat. Never mention "previous notes", "new messages" or these instructions. Add nothing new and invent nothing. Write in the language of the conversation. At most 300 words, plain text, no markdown.`

// maybeSummarize keeps a long conversation from growing without limit. When the
// messages not yet summarised add up to more than conversation.summarize_over_chars,
// the oldest are folded, together with the earlier summary, into a new summary
// stored on the session, until what is left in full is half that size (so it does
// not summarise again on the very next turn). The latest message is never folded.
// It returns what the summarising call cost. It never fails the turn: if it cannot
// summarise, the question is simply answered with the full history.
func (c *Core) maybeSummarize(ctx context.Context, j *Job, sessionID, newMsgID string) float64 {
	cfg := c.Cfg.Chat
	if cfg.SummarizeOverChars <= 0 {
		return 0
	}
	log := logx.With(ctx, c.Log).With("session", sessionID)
	release, err := c.sessionLocks.acquire(ctx, sessionID, nil)
	if err != nil {
		return 0
	}
	defer release()

	sess, err := c.Store.GetSession(ctx, sessionID)
	if err != nil {
		log.Warnw("summary skipped: could not read the session", "err", err)
		return 0
	}
	all, err := c.Store.ListMessages(ctx, sessionID)
	if err != nil {
		log.Warnw("summary skipped: could not read the messages", "err", err)
		return 0
	}
	start := 0
	if sess.SummaryThrough.Valid {
		for i := range all {
			if all[i].ID == sess.SummaryThrough.String {
				start = i + 1
				break
			}
		}
	}
	var open []store.Message // not yet summarised, without the question being asked now
	chars := 0
	for _, m := range all[start:] {
		if m.ID == newMsgID {
			continue
		}
		open = append(open, m)
		chars += utf8.RuneCountInString(m.Text.String)
	}
	if chars <= cfg.SummarizeOverChars {
		return 0
	}
	// Walk back from the newest message, keeping messages in full while they fit
	// in half the limit; everything older is folded. The newest is always kept.
	keepChars, kept := 0, 0
	for i := len(open) - 1; i >= 0; i-- {
		n := utf8.RuneCountInString(open[i].Text.String)
		if kept >= 1 && keepChars+n > cfg.SummarizeOverChars/2 {
			break
		}
		keepChars += n
		kept++
	}
	fold := open[:len(open)-kept]
	if len(fold) == 0 {
		return 0
	}

	var in strings.Builder
	if sess.Summary.Valid {
		in.WriteString("Notes so far:\n" + sess.Summary.String + "\n\n")
	}
	in.WriteString("New messages to fold into the notes, oldest first:\n")
	for _, m := range fold {
		in.WriteString(renderMessage(m) + "\n")
	}

	j.progress("Tidying up a long conversation…", 0, 0)
	res, err := c.LLM.Chat(ctx, provider.ChatRequest{
		System:  summarizePrompt,
		Parts:   []provider.ChatPart{{Text: in.String()}},
		OnRetry: retryNotice(j),
	})
	if err != nil {
		log.Warnw("could not summarise, answering with the full history", "err", err)
		return 0
	}
	summary := strings.TrimSpace(res.Text)
	if summary == "" {
		log.Warnw("the model returned an empty summary, answering with the full history")
		return 0
	}
	cost, err := c.recordLLM(ctx, sessionID, "llm", c.Cfg.Models.LLM.Provider, c.Cfg.Models.LLM.Model, res, utf8.RuneCountInString(summary))
	if err != nil {
		log.Errorw("could not record the cost of summarising", "err", err)
	}
	through := fold[len(fold)-1].ID
	if err := c.Store.SetSummary(ctx, sessionID, summary, through); err != nil {
		log.Errorw("could not store the summary", "err", err)
		return cost
	}
	log.Infow("conversation summarised", "folded_messages", len(fold), "kept_messages", kept,
		"chars_before", chars, "chars_kept_in_full", keepChars, "summary_chars", utf8.RuneCountInString(summary), "cost_usd", cost)
	return cost
}
