package objectstore

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestKeyIsChronological(t *testing.T) {
	var keys []string
	for range 5 {
		keys = append(keys, Key(".mp3"))
		time.Sleep(2 * time.Millisecond)
	}
	if !sort.StringsAreSorted(keys) {
		t.Errorf("keys not in time order: %v", keys)
	}
	if !strings.HasSuffix(keys[0], "Z.mp3") || strings.Contains(keys[0], "/") {
		t.Errorf("unexpected key shape %q", keys[0])
	}
}

func TestLocal(t *testing.T) {
	ctx := context.Background()
	l := Local{Root: t.TempDir()}
	if ok, _ := l.Exists(ctx, "a.mp3"); ok {
		t.Error("exists before put")
	}
	if err := l.Put(ctx, "a.mp3", []byte("x"), "audio/mpeg"); err != nil {
		t.Fatal(err)
	}
	if b, err := l.Get(ctx, "a.mp3"); err != nil || string(b) != "x" {
		t.Errorf("get: %q %v", b, err)
	}
	if ok, _ := l.Exists(ctx, "a.mp3"); !ok {
		t.Error("should exist")
	}
	if _, err := l.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if _, err := l.PresignGet(ctx, "a.mp3", time.Hour); !errors.Is(err, ErrNoPresign) {
		t.Errorf("want ErrNoPresign, got %v", err)
	}
}

// Presigning is local signing: no network and no R2 request.
func TestR2PresignIsOffline(t *testing.T) {
	r, err := NewR2("https://acct.r2.cloudflarestorage.com", "bkt", "id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	u, err := r.PresignGet(context.Background(), "2026-10-30T14-22-35.000001Z.mp3", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"https://", "acct.r2.cloudflarestorage.com", "bkt", "X-Amz-Signature", "X-Amz-Expires=3600"} {
		if !strings.Contains(u, want) {
			t.Errorf("url missing %q: %s", want, u)
		}
	}
	if _, err := NewR2("", "b", "i", "s"); err == nil {
		t.Error("empty endpoint should fail")
	}
}
