// Package objectstore keeps audio files outside the runner. Backends implement
// Store (local disk for dev, Cloudflare R2 for prod, more later); callers only
// hold keys and ask for a presigned URL when someone needs to play the file.
package objectstore

import (
	"context"
	"errors"
	"time"

	"github.com/spf13/viper"
)

// Store is one object storage backend. Add a new one by implementing it and
// adding a case to FromSettings.
type Store interface {
	// Kind names the backend ("local", "r2", ...) for logs and errors.
	Kind() string
	// Put stores data under key.
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Get returns the object, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)
	// Exists reports whether key is stored.
	Exists(ctx context.Context, key string) (bool, error)
	// PresignGet returns a time-limited URL that plays/downloads key without
	// credentials. Signing is local: it makes no request and costs nothing.
	// Backends with no URLs return ErrNoPresign.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

var (
	ErrNotFound  = errors.New("object not found")
	ErrNoPresign = errors.New("this object store cannot make URLs")
)

// Key names a new object by its UTC creation time, so listing the bucket sorts
// chronologically (date, then time). ext includes the dot, e.g. ".mp3".
func Key(ext string) string {
	return time.Now().UTC().Format("2006-01-02T15-04-05.000000Z") + ext
}

// FromSettings picks the backend from config: app.dev true -> local disk under
// app.data_dir/audio; otherwise Cloudflare R2, configured only by secrets:
// R2_ENDPOINT, R2_BUCKET, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY.
func FromSettings(cfg, sec *viper.Viper) (Store, error) {
	if cfg.GetBool("app.dev") {
		return Local{Root: cfg.GetString("app.data_dir") + "/audio"}, nil
	}
	return NewR2(sec.GetString("R2_ENDPOINT"), sec.GetString("R2_BUCKET"),
		sec.GetString("R2_ACCESS_KEY_ID"), sec.GetString("R2_SECRET_ACCESS_KEY"))
}
