// Package store is the Postgres persistence layer. SQL lives in sql/*.sql and is
// compiled to typed Go by sqlc (see sqlc.yaml); no SQL strings in Go code
// except the migration runner.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"sevi/internal/model"
	"sevi/internal/store/db"
)

//go:embed sql/migrations/*.sql
var migrationFS embed.FS

var _ model.Store = (*Store)(nil)

type Store struct {
	sql *sql.DB
	q   *db.Queries

	retentionDays int // 0 = keep everything (see SetRetention)
}

// Open connects to the Postgres database at dsn and applies migrations. It is
// tuned for Neon's free plan, which suspends compute after 5 idle minutes and
// wakes it on the next connection (about a second):
//   - idle connections are closed after 4 minutes, before Neon would drop them
//     from under us, so a pooled connection is never silently dead;
//   - the first connection is retried while the database wakes;
//   - statements are described, not prepared by name, so the pooled (PgBouncer)
//     endpoint works as well as the direct one.
func Open(ctx context.Context, dsn string, log *zap.SugaredLogger) (*Store, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 20 * time.Second
	}
	conn := stdlib.OpenDB(*cfg)
	conn.SetMaxOpenConns(3) // one Cloud Run instance; a few queries at once is plenty
	conn.SetMaxIdleConns(3)
	conn.SetConnMaxIdleTime(4 * time.Minute)
	conn.SetConnMaxLifetime(30 * time.Minute)
	if err := pingWithRetry(ctx, conn, log); err != nil {
		conn.Close()
		return nil, err
	}
	if err := migrate(ctx, conn, log); err != nil {
		conn.Close()
		return nil, err
	}
	return &Store{sql: conn, q: db.New(conn)}, nil
}

// pingWithRetry tries a few times: a suspended Neon database can refuse or time
// out the first connection while it starts.
func pingWithRetry(ctx context.Context, conn *sql.DB, log *zap.SugaredLogger) error {
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		if err = conn.PingContext(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		log.Warnw("database not reachable yet, retrying", "attempt", attempt, "err", err)
		select {
		case <-time.After(time.Duration(attempt) * time.Second):
		case <-ctx.Done():
		}
	}
	return fmt.Errorf("connect to db: %w", err)
}

// Ping runs a trivial query: it checks the database is up and, on Neon, keeps
// its compute awake.
func (s *Store) Ping(ctx context.Context) error { return s.sql.PingContext(ctx) }

func (s *Store) Close() error { return s.sql.Close() }

// migrationLockID is the Postgres advisory lock that serialises migrations when
// two instances start at once.
const migrationLockID = 727265

func migrate(ctx context.Context, pool *sql.DB, log *zap.SugaredLogger) error {
	// The advisory lock is per connection, so hold one connection throughout.
	conn, err := pool.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at BIGINT NOT NULL DEFAULT extract(epoch FROM now())::bigint)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	files, err := fs.Glob(migrationFS, "sql/migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version := filepath.Base(f)
		var one int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = $1`, version).Scan(&one)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		body, err := migrationFS.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Infow("applied migration", "version", version)
	}
	return nil
}

// URLHash is the cache key for a URL.
func URLHash(url string) string {
	h := sha256.Sum256([]byte(url))
	return hex.EncodeToString(h[:])
}

// newID returns a time-ordered UUID (v7) as text, used as every row's primary key.
func newID() string { return uuid.Must(uuid.NewV7()).String() }

// ErrNotFound is returned by lookups that find nothing.
var ErrNotFound = errors.New("not found")

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) CreateSession(ctx context.Context, kind, url string) (model.Session, error) {
	return s.q.CreateSession(ctx, db.CreateSessionParams{ID: newID(), Kind: kind, Url: sql.NullString{String: url, Valid: url != ""}})
}

func (s *Store) GetSession(ctx context.Context, id string) (model.Session, error) {
	r, err := s.q.GetSession(ctx, id)
	return r, notFound(err)
}

func (s *Store) AddLedger(ctx context.Context, e model.LedgerEntry) (string, error) {
	p := db.InsertLedgerParams{
		ID: newID(), SessionID: e.SessionID, Role: e.Role, Provider: e.Provider, Model: e.Model,
		GenerationID: sql.NullString{String: e.GenerationID, Valid: e.GenerationID != ""},
		Chars:        int64(e.Chars),
		InputUnits:   sql.NullInt64{Int64: int64(e.InputUnits), Valid: true},
		OutputUnits:  sql.NullInt64{Int64: int64(e.OutputUnits), Valid: true},
	}
	if e.CostUSD != nil {
		p.CostUsd = sql.NullFloat64{Float64: *e.CostUSD, Valid: true}
	}
	return s.q.InsertLedger(ctx, p)
}

func (s *Store) SetLedgerUsage(ctx context.Context, id string, inTok, outTok int, costUSD float64) error {
	return s.q.SetLedgerUsage(ctx, db.SetLedgerUsageParams{
		InputUnits:  sql.NullInt64{Int64: int64(inTok), Valid: true},
		OutputUnits: sql.NullInt64{Int64: int64(outTok), Valid: true},
		CostUsd:     sql.NullFloat64{Float64: costUSD, Valid: true},
		ID:          id,
	})
}

func (s *Store) ListLedger(ctx context.Context, sessionID string) ([]model.LedgerRow, error) {
	return s.q.ListLedgerBySession(ctx, sessionID)
}

func (s *Store) SessionCost(ctx context.Context, sessionID string) (model.SessionCost, error) {
	return s.q.SessionCost(ctx, sessionID)
}

func (s *Store) GetPage(ctx context.Context, urlHash string) (model.Page, error) {
	r, err := s.q.GetPage(ctx, urlHash)
	return r, notFound(err)
}

func (s *Store) PutPage(ctx context.Context, url, title, text string) error {
	return s.q.PutPage(ctx, db.PutPageParams{
		ID: newID(), UrlHash: URLHash(url), Url: url, Text: text,
		Title: sql.NullString{String: title, Valid: title != ""},
	})
}

func (s *Store) GetAudio(ctx context.Context, urlHash, ttsModel, voice string) (model.CachedAudio, error) {
	r, err := s.q.GetAudio(ctx, db.GetAudioParams{UrlHash: urlHash, TtsModel: ttsModel, Voice: voice})
	return r, notFound(err)
}

func (s *Store) PutAudio(ctx context.Context, urlHash, ttsModel, voice, audioKey string, seconds int) error {
	return s.q.PutAudio(ctx, db.PutAudioParams{ID: newID(), UrlHash: urlHash, TtsModel: ttsModel, Voice: voice, AudioKey: audioKey, Seconds: int64(seconds)})
}

func nullStr(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

func (s *Store) AddMessage(ctx context.Context, m model.NewMessage) (model.Message, error) {
	return s.q.CreateMessage(ctx, db.CreateMessageParams{
		ID: newID(), SessionID: m.SessionID, Role: m.Role, Text: nullStr(m.Text), AudioKey: nullStr(m.AudioKey),
		ParentID: nullStr(m.ParentID), Format: nullStr(m.Format),
	})
}

// SetMessageText fills in a message's text, e.g. the transcript of a voice note.
func (s *Store) SetMessageText(ctx context.Context, id, text string) error {
	return s.q.SetMessageText(ctx, db.SetMessageTextParams{Text: nullStr(text), ID: id})
}

func (s *Store) GetMessage(ctx context.Context, id string) (model.Message, error) {
	r, err := s.q.GetMessage(ctx, id)
	return r, notFound(err)
}

func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]model.Message, error) {
	return s.q.ListMessagesBySession(ctx, sessionID)
}

func (s *Store) AddPending(ctx context.Context, sessionID, kind, payload string) (model.Pending, error) {
	return s.q.CreatePending(ctx, db.CreatePendingParams{ID: newID(), SessionID: sessionID, Kind: kind, Payload: payload})
}

// OpenPending returns the newest unanswered question for a session, or ErrNotFound.
func (s *Store) OpenPending(ctx context.Context, sessionID string) (model.Pending, error) {
	r, err := s.q.GetOpenPending(ctx, sessionID)
	return r, notFound(err)
}

// ResolvePending marks a pending question confirmed or cancelled.
func (s *Store) ResolvePending(ctx context.Context, id, status string) error {
	return s.q.SetPendingStatus(ctx, db.SetPendingStatusParams{Status: status, ID: id})
}

// PutThread records that an adapter's handle for a message (e.g. Telegram
// chat_id:message_id) is a stored message, so a reply to it finds its session.
// Re-registering a handle moves it.
func (s *Store) PutThread(ctx context.Context, adapter, threadRef, messageID string) error {
	return s.q.PutThread(ctx, db.PutThreadParams{ID: newID(), Adapter: adapter, ThreadRef: threadRef, MessageID: messageID})
}

// MessageForThread returns the message an adapter handle points at, or ErrNotFound.
func (s *Store) MessageForThread(ctx context.Context, adapter, threadRef string) (model.Message, error) {
	r, err := s.q.GetThreadMessage(ctx, db.GetThreadMessageParams{Adapter: adapter, ThreadRef: threadRef})
	return r, notFound(err)
}

func (s *Store) GetPrice(ctx context.Context, provider, modelName string) (model.ModelPrice, error) {
	r, err := s.q.GetPrice(ctx, db.GetPriceParams{Provider: provider, Model: modelName})
	if err != nil {
		return model.ModelPrice{}, notFound(err)
	}
	return model.ModelPrice{Prompt: r.PromptUsd, Completion: r.CompletionUsd, Audio: r.AudioUsd, CacheRead: r.CacheReadUsd, FetchedOn: r.FetchedOn}, nil
}

func (s *Store) PutPrice(ctx context.Context, provider, modelName string, p model.ModelPrice) error {
	return s.q.UpsertPrice(ctx, db.UpsertPriceParams{
		ID: newID(), Provider: provider, Model: modelName,
		PromptUsd: p.Prompt, CompletionUsd: p.Completion, AudioUsd: p.Audio, CacheReadUsd: p.CacheRead, FetchedOn: p.FetchedOn,
	})
}

// PricedModels lists the models of a provider that have a stored price.
func (s *Store) PricedModels(ctx context.Context, provider string) ([]string, error) {
	return s.q.ListPricedModels(ctx, provider)
}

// LatestCostedCall is the newest ledger row of a model that has both a
// provider generation id and a cost; ErrNotFound if there is none.
func (s *Store) LatestCostedCall(ctx context.Context, provider, modelName string) (model.LedgerRow, error) {
	r, err := s.q.LatestCostedLedger(ctx, db.LatestCostedLedgerParams{Provider: provider, Model: modelName})
	return r, notFound(err)
}

// SetMessageAudio records where a message's audio is stored.
func (s *Store) SetMessageAudio(ctx context.Context, id, audioKey string) error {
	return s.q.SetMessageAudio(ctx, db.SetMessageAudioParams{AudioKey: nullStr(audioKey), ID: id})
}

// SetSummary stores the rolling summary of a session and the id of the last
// message it covers.
func (s *Store) SetSummary(ctx context.Context, sessionID, summary, throughMessageID string) error {
	return s.q.SetSessionSummary(ctx, db.SetSessionSummaryParams{Summary: nullStr(summary), SummaryThrough: nullStr(throughMessageID), ID: sessionID})
}

// MarkSeen records that the adapter has handled the message with this ref and
// reports whether this is the first time. Rows older than a week are dropped.
func (s *Store) MarkSeen(ctx context.Context, adapter, ref string) (bool, error) {
	n, err := s.q.MarkSeen(ctx, db.MarkSeenParams{ID: newID(), Adapter: adapter, Ref: ref})
	if err != nil {
		return false, err
	}
	_ = s.Prune(ctx) // housekeeping; a failure must not drop the message
	return n > 0, nil
}

// SetRetention sets how many days data is kept (retention_days in config.json);
// 0 keeps everything. Call it once after Open.
func (s *Store) SetRetention(days int) { s.retentionDays = days }

// Prune deletes data older than the retention period, but only when the last
// prune (recorded in the maintenance table) is itself older than that period.
// It rides on incoming messages: there is no job, and a sleeping instance costs
// nothing. It deletes whole conversations (messages, links, ledger rows), cached
// pages and audio rows, and seen ids, and records the time, in one statement.
func (s *Store) Prune(ctx context.Context) error {
	if s.retentionDays <= 0 {
		return nil
	}
	return s.q.PruneOld(ctx, int32(s.retentionDays))
}
