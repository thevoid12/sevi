// Package reqid carries a request id through context so one failure can be
// traced across adapter, core and provider log lines.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type ctxKey struct{}

func New() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the request id in ctx, or "" if none.
func From(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
