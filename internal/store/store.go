// Package store is the SQLite persistence layer. SQL lives in sql/*.sql and is
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
	"os"
	"path/filepath"
	"sort"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3" // cgo driver; needs gcc to build
	"go.uber.org/zap"

	"sevi/internal/store/db"
)

//go:embed sql/migrations/*.sql
var migrationFS embed.FS

// Re-exported row types so callers don't import the generated package.
type (
	Session     = db.Session
	LedgerRow   = db.Ledger
	Page        = db.PageCache
	CachedAudio = db.AudioCache
	SessionCost = db.SessionCostRow
)

type Store struct {
	sql *sql.DB
	q   *db.Queries
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(ctx context.Context, path string, log *zap.SugaredLogger) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	dsn := "file:" + path + "?_foreign_keys=1&_journal_mode=WAL&_busy_timeout=5000"
	conn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	conn.SetMaxOpenConns(1) // SQLite: one writer; avoids "database is locked"
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	if err := migrate(ctx, conn, log); err != nil {
		conn.Close()
		return nil, err
	}
	return &Store{sql: conn, q: db.New(conn)}, nil
}

func (s *Store) Close() error { return s.sql.Close() }

func migrate(ctx context.Context, conn *sql.DB, log *zap.SugaredLogger) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
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
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&one)
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
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

func (s *Store) CreateSession(ctx context.Context, kind, url string) (Session, error) {
	return s.q.CreateSession(ctx, db.CreateSessionParams{ID: newID(), Kind: kind, Url: sql.NullString{String: url, Valid: url != ""}})
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	r, err := s.q.GetSession(ctx, id)
	return r, notFound(err)
}

// LedgerEntry is a provider call that just succeeded; usage and cost come later.
type LedgerEntry struct {
	SessionID    string
	Role         string // llm | stt | tts
	Provider     string
	Model        string
	GenerationID string
	Chars        int
}

func (s *Store) AddLedger(ctx context.Context, e LedgerEntry) (string, error) {
	return s.q.InsertLedger(ctx, db.InsertLedgerParams{
		ID: newID(), SessionID: e.SessionID, Role: e.Role, Provider: e.Provider, Model: e.Model,
		GenerationID: sql.NullString{String: e.GenerationID, Valid: e.GenerationID != ""},
		Chars:        int64(e.Chars),
	})
}

func (s *Store) SetLedgerUsage(ctx context.Context, id string, inTok, outTok int, costUSD float64) error {
	return s.q.SetLedgerUsage(ctx, db.SetLedgerUsageParams{
		InputUnits:  sql.NullInt64{Int64: int64(inTok), Valid: true},
		OutputUnits: sql.NullInt64{Int64: int64(outTok), Valid: true},
		CostUsd:     sql.NullFloat64{Float64: costUSD, Valid: true},
		ID:          id,
	})
}

func (s *Store) ListLedger(ctx context.Context, sessionID string) ([]LedgerRow, error) {
	return s.q.ListLedgerBySession(ctx, sessionID)
}

func (s *Store) SessionCost(ctx context.Context, sessionID string) (SessionCost, error) {
	return s.q.SessionCost(ctx, sessionID)
}

func (s *Store) GetPage(ctx context.Context, urlHash string) (Page, error) {
	r, err := s.q.GetPage(ctx, urlHash)
	return r, notFound(err)
}

func (s *Store) PutPage(ctx context.Context, url, title, text string) error {
	return s.q.PutPage(ctx, db.PutPageParams{
		ID: newID(), UrlHash: URLHash(url), Url: url, Text: text,
		Title: sql.NullString{String: title, Valid: title != ""},
	})
}

func (s *Store) GetAudio(ctx context.Context, urlHash, ttsModel, voice string) (CachedAudio, error) {
	r, err := s.q.GetAudio(ctx, db.GetAudioParams{UrlHash: urlHash, TtsModel: ttsModel, Voice: voice})
	return r, notFound(err)
}

func (s *Store) PutAudio(ctx context.Context, urlHash, ttsModel, voice, audioKey string, seconds int) error {
	return s.q.PutAudio(ctx, db.PutAudioParams{ID: newID(), UrlHash: urlHash, TtsModel: ttsModel, Voice: voice, AudioKey: audioKey, Seconds: int64(seconds)})
}

type (
	Message = db.Message
	Pending = db.Pending
)

func (s *Store) AddMessage(ctx context.Context, sessionID, role, text, audioKey string) (Message, error) {
	return s.q.CreateMessage(ctx, db.CreateMessageParams{
		ID: newID(), SessionID: sessionID, Role: role,
		Text:     sql.NullString{String: text, Valid: text != ""},
		AudioKey: sql.NullString{String: audioKey, Valid: audioKey != ""},
	})
}

func (s *Store) GetMessage(ctx context.Context, id string) (Message, error) {
	r, err := s.q.GetMessage(ctx, id)
	return r, notFound(err)
}

func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]Message, error) {
	return s.q.ListMessagesBySession(ctx, sessionID)
}

func (s *Store) AddPending(ctx context.Context, sessionID, kind, payload string) (Pending, error) {
	return s.q.CreatePending(ctx, db.CreatePendingParams{ID: newID(), SessionID: sessionID, Kind: kind, Payload: payload})
}

// OpenPending returns the newest unanswered question for a session, or ErrNotFound.
func (s *Store) OpenPending(ctx context.Context, sessionID string) (Pending, error) {
	r, err := s.q.GetOpenPending(ctx, sessionID)
	return r, notFound(err)
}

// ResolvePending marks a pending question confirmed or cancelled.
func (s *Store) ResolvePending(ctx context.Context, id, status string) error {
	return s.q.SetPendingStatus(ctx, db.SetPendingStatusParams{Status: status, ID: id})
}

// TTSCostPerChar is the observed average TTS cost per character for a model,
// from ledger rows whose cost is known. ok is false when there is no history.
func (s *Store) TTSCostPerChar(ctx context.Context, model string) (perChar float64, ok bool, err error) {
	r, err := s.q.TTSCostTotals(ctx, model)
	if err != nil {
		return 0, false, err
	}
	if r.TotalChars == 0 || r.TotalUsd == 0 {
		return 0, false, nil
	}
	return r.TotalUsd / float64(r.TotalChars), true, nil
}

// PutThread records that an adapter's conversation handle (e.g. a Telegram
// chat_id:message_id) belongs to a session. Re-registering a handle moves it.
func (s *Store) PutThread(ctx context.Context, adapter, threadRef, sessionID string) error {
	return s.q.PutThread(ctx, db.PutThreadParams{ID: newID(), Adapter: adapter, ThreadRef: threadRef, SessionID: sessionID})
}

// SessionForThread returns the session an adapter handle belongs to, or ErrNotFound.
func (s *Store) SessionForThread(ctx context.Context, adapter, threadRef string) (string, error) {
	r, err := s.q.GetThread(ctx, db.GetThreadParams{Adapter: adapter, ThreadRef: threadRef})
	return r.SessionID, notFound(err)
}
