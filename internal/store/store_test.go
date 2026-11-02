package store_test

import (
	"context"
	"database/sql"
	"testing"

	"go.uber.org/zap"

	"sevi/internal/model"
	"sevi/internal/store/storetest"
)

func TestMarkSeenIsTrueOnlyTheFirstTime(t *testing.T) {
	st := storetest.Open(t, zap.NewNop().Sugar())
	ctx := context.Background()
	first, err := st.MarkSeen(ctx, "telegram", "100")
	if err != nil || !first {
		t.Fatalf("first sight: %v %v", first, err)
	}
	if again, err := st.MarkSeen(ctx, "telegram", "100"); err != nil || again {
		t.Errorf("repeat should not be first: %v %v", again, err)
	}
	if other, _ := st.MarkSeen(ctx, "telegram", "101"); !other {
		t.Error("a different ref is new")
	}
	if web, _ := st.MarkSeen(ctx, "web", "100"); !web {
		t.Error("the same ref from another adapter is new")
	}
}

func TestPruneDeletesOldDataOnly(t *testing.T) {
	st, raw := storetest.OpenSQL(t, zap.NewNop().Sugar())
	ctx := context.Background()
	st.SetRetention(7)

	// An old conversation with every kind of child row, and a fresh one.
	old, _ := st.CreateSession(ctx, "blog", "https://old.example")
	oldMsg, _ := st.AddMessage(ctx, model.NewMessage{SessionID: old.ID, Role: "user", Text: "hi"})
	st.AddMessage(ctx, model.NewMessage{SessionID: old.ID, Role: "assistant", Text: "yo", ParentID: oldMsg.ID})
	st.PutThread(ctx, "telegram", "1", oldMsg.ID)
	st.AddPending(ctx, old.ID, "confirm", "{}")
	st.AddLedger(ctx, model.LedgerEntry{SessionID: old.ID, Role: "llm", Provider: "p", Model: "m"})
	fresh, _ := st.CreateSession(ctx, "blog", "https://fresh.example")
	st.AddMessage(ctx, model.NewMessage{SessionID: fresh.ID, Role: "user", Text: "hello"})
	st.PutPage(ctx, "https://old.example", "t", "text")
	st.PutPage(ctx, "https://fresh.example", "t", "text")
	st.PutAudio(ctx, "oldhash", "m", "v", "k1", 1)
	st.PutAudio(ctx, "freshhash", "m", "v", "k2", 1)
	st.PutPartial(ctx, "oldhash", "m", "v", 3, 1, "oldkey", 0.5)
	st.PutPartial(ctx, "freshhash", "m", "v", 3, 1, "freshkey", 0.5)
	st.MarkSeen(ctx, "telegram", "old")
	st.MarkSeen(ctx, "telegram", "fresh")

	// Age the "old" rows to ten days.
	ago := `now() - interval '10 days'`
	for _, q := range []string{
		`UPDATE sessions SET created_on = ` + ago + `, updated_on = ` + ago + ` WHERE id = '` + old.ID + `'`,
		`UPDATE messages SET created_on = ` + ago + `, updated_on = ` + ago + ` WHERE session_id = '` + old.ID + `'`,
		`UPDATE page_cache SET created_on = ` + ago + `, updated_on = ` + ago + ` WHERE url = 'https://old.example'`,
		`UPDATE audio_cache SET created_on = ` + ago + ` WHERE url_hash = 'oldhash'`,
		`UPDATE partial_audio SET created_on = ` + ago + ` WHERE url_hash = 'oldhash'`,
		`UPDATE seen_updates SET created_on = ` + ago + ` WHERE ref = 'old'`,
	} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	// MarkSeen above already did the first prune (nothing was old yet); make it due.
	raw.ExecContext(ctx, `UPDATE maintenance SET last_pruned_on = NULL`)
	if err := st.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var lastPruned sql.NullTime
	raw.QueryRowContext(ctx, `SELECT last_pruned_on FROM maintenance WHERE id = 'prune'`).Scan(&lastPruned)
	if !lastPruned.Valid {
		t.Error("the prune time must be recorded")
	}
	count := func(table string) int {
		var n int
		raw.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n)
		return n
	}
	// Only the fresh conversation, page, audio row and seen id remain.
	for table, want := range map[string]int{
		"sessions": 1, "messages": 1, "thread_map": 0, "pending": 0, "ledger": 0,
		"page_cache": 1, "audio_cache": 1, "partial_audio": 1, "seen_updates": 1,
	} {
		if got := count(table); got != want {
			t.Errorf("%s: %d rows, want %d", table, got, want)
		}
	}

	// Not due again yet: an old row added now survives the next call.
	late, _ := st.CreateSession(ctx, "blog", "")
	raw.ExecContext(ctx, `UPDATE sessions SET created_on = `+ago+`, updated_on = `+ago+` WHERE id = '`+late.ID+`'`)
	st.Prune(ctx)
	if got := count("sessions"); got != 2 {
		t.Errorf("a prune that is not due must delete nothing: %d sessions", got)
	}
	// Make it due again (last prune 10 days ago), and now it goes.
	raw.ExecContext(ctx, `UPDATE maintenance SET last_pruned_on = `+ago)
	st.Prune(ctx)
	if got := count("sessions"); got != 1 {
		t.Errorf("a due prune must delete the old session: %d sessions", got)
	}

	// A conversation with an old session row but a new message stays.
	keep, _ := st.CreateSession(ctx, "blog", "")
	raw.ExecContext(ctx, `UPDATE sessions SET created_on = `+ago+`, updated_on = `+ago+` WHERE id = '`+keep.ID+`'`)
	st.AddMessage(ctx, model.NewMessage{SessionID: keep.ID, Role: "user", Text: "still talking"})
	raw.ExecContext(ctx, `UPDATE maintenance SET last_pruned_on = `+ago)
	st.Prune(ctx)
	if got := count("sessions"); got != 2 {
		t.Errorf("an active conversation must survive: %d sessions", got)
	}

	// Retention 0 keeps everything.
	st.SetRetention(0)
	raw.ExecContext(ctx, `UPDATE sessions SET created_on = `+ago+`, updated_on = `+ago)
	raw.ExecContext(ctx, `UPDATE messages SET created_on = `+ago+`, updated_on = `+ago)
	raw.ExecContext(ctx, `UPDATE maintenance SET last_pruned_on = `+ago)
	st.Prune(ctx)
	if got := count("sessions"); got != 2 {
		t.Errorf("retention 0 must not delete: %d sessions", got)
	}
}
