// Package storetest opens a throwaway Postgres schema for tests, so each test
// gets an empty database without touching the dev data.
package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"sevi/internal/store"
)

// DefaultURL matches localdb/docker-compose.yml.
const DefaultURL = "postgres://sevi:sevi@127.0.0.1:5432/sevi?sslmode=disable"

// Open returns a Store in its own schema, dropped when the test ends. The test
// is skipped if Postgres is not reachable (start it with localdb/docker-compose.yml).
// Set SEVI_TEST_DATABASE_URL to use another server.
func Open(t *testing.T, log *zap.SugaredLogger) *store.Store {
	t.Helper()
	st, _ := OpenSQL(t, log)
	return st
}

// OpenSQL is Open plus a plain connection to the same schema, for tests that
// need to write rows directly (for example with old timestamps).
func OpenSQL(t *testing.T, log *zap.SugaredLogger) (*store.Store, *sql.DB) {
	t.Helper()
	base := os.Getenv("SEVI_TEST_DATABASE_URL")
	if base == "" {
		base = DefaultURL
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", base)
	if err == nil {
		err = admin.PingContext(ctx)
	}
	if err != nil {
		t.Skipf("postgres not reachable (docker compose -f localdb/docker-compose.yml up -d): %v", err)
	}
	schema := "t_" + uuid.NewString()[:8]
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA %s CASCADE`, schema))
		admin.Close()
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err := store.Open(ctx, u.String(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	raw, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return st, raw
}
