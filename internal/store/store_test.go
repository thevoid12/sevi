package store_test

import (
	"context"
	"testing"

	"go.uber.org/zap"

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
