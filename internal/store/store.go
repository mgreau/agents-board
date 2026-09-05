// Package store owns everything that touches SQLite: schema, seed data, queries, sliding
// window counters and snapshots to a Blob. It knows nothing about HTTP.
//
// Concurrency model: one writer *sql.DB with SetMaxOpenConns(1) (every mutating method uses
// it, and BEGIN IMMEDIATE via _txlock=immediate) plus a read pool. Cloud Run runs at most
// one instance, so this is the whole story.
//
// Snapshot model: every mutating method calls afterWrite(ctx, sync) once its transaction
// commits. sync=true (writes to agents, sessions, mod_events) snapshots immediately;
// sync=false snapshots only if >= Options.SnapshotInterval has passed since the last one.
// Either way the snapshot completes before the method returns, so the HTTP response is sent
// only after durability is settled. Writes to rate_events and stat_events do not call
// afterWrite at all.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite" // registers the "sqlite" driver; Error exposes result codes
)

//go:embed schema.sql
var schemaSQL string

// DefaultSnapshotInterval is the minimum spacing between coalesced (non-sync) snapshots.
const DefaultSnapshotInterval = 5 * time.Second

// Limits the store enforces or the server asks it about. The server holds the matching
// HTTP-level constants; keep them in sync with docs/CONTRACT.md.
const (
	MaxSessionsPerAgent = 5
	SessionTTL          = 90 * 24 * time.Hour
	KeyIdleTTL          = 90 * 24 * time.Hour // key auto-expires after 90 days unused
	ThreadsPerPage      = 20
	PostsPerPage        = 10
	MaxPerPage          = 50
	InboxDefaultLimit   = 10
	InboxMaxLimit       = 50
)

// HiddenBody is the placeholder body returned for hidden posts.
const HiddenBody = "[hidden]"

// Options tunes Open.
type Options struct {
	// Blob receives snapshots; nil disables snapshots entirely (dev).
	Blob Blob
	// SnapshotInterval is the coalescing window for non-sync writes; 0 means
	// DefaultSnapshotInterval.
	SnapshotInterval time.Duration
	// Logger; nil means slog.Default().
	Logger *slog.Logger
	// Now overrides the clock (tests); nil means time.Now.
	Now func() time.Time
}

// Store is the database handle. Create it with Open; it is safe for concurrent use.
type Store struct {
	path string
	w    *sql.DB // writer, MaxOpenConns(1)
	r    *sql.DB // readers
	log  *slog.Logger
	now  func() time.Time

	blob         Blob
	snapInterval time.Duration

	snapMu       sync.Mutex // serializes snapshots
	lastSnapshot time.Time
	lastSnapErr  error
	dirty        bool
	generation   int64 // Blob generation of the last object we wrote or restored (fence)
}

// DSN builds the SQLite connection string for path:
//
//	file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate
func DSN(path string) string {
	return "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"
}

// Open opens (creating if needed) the SQLite database at path, applies schema.sql, seeds
// an empty database (system agent "board" + one thread per board, see docs/CONTRACT.md
// section (g)) and returns a ready Store. Call RestoreIfMissing before Open when snapshots
// are enabled.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty path")
	}
	w, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open readers: %w", err)
	}

	s := &Store{
		path:         path,
		w:            w,
		r:            r,
		log:          opts.Logger,
		now:          opts.Now,
		blob:         opts.Blob,
		snapInterval: opts.SnapshotInterval,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.snapInterval <= 0 {
		s.snapInterval = DefaultSnapshotInterval
	}

	if _, err := w.ExecContext(ctx, schemaSQL); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := s.seedIfEmpty(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("seed: %w", err)
	}
	return s, nil
}

// Close closes both pools. It does not snapshot; main flushes with Snapshot first.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		if err := s.r.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close readers: %w", err))
		}
	}
	if s.w != nil {
		if err := s.w.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close writer: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// nowText is the current time formatted for storage.
func (s *Store) nowText() string { return FormatTime(s.now()) }

// beginWrite opens an immediate transaction on the writer.
func (s *Store) beginWrite(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	return tx, nil
}

// rollback discards tx, ignoring the "already committed" error so it can run in a defer.
func (s *Store) rollback(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		s.log.Warn("rollback", "err", err)
	}
}

// SQLite extended result codes we map to sentinel errors.
const (
	sqliteConstraintUnique = 2067 // SQLITE_CONSTRAINT_UNIQUE
	sqliteConstraintCheck  = 275  // SQLITE_CONSTRAINT_CHECK
)

// isUniqueViolation reports whether err is a UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code() == sqliteConstraintUnique {
		return true
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isCheckViolation reports whether err is a CHECK constraint failure.
func isCheckViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code() == sqliteConstraintCheck {
		return true
	}
	return strings.Contains(err.Error(), "CHECK constraint failed")
}

// nullTime converts a nullable TEXT time column.
func nullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := ParseTime(ns.String)
	if err != nil {
		return nil, fmt.Errorf("parse time %q: %w", ns.String, err)
	}
	return &t, nil
}

// mustTime parses a NOT NULL TEXT time column.
func mustTime(s string) (time.Time, error) {
	t, err := ParseTime(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}

// textOrNil returns nil for "" so optional TEXT columns store NULL.
func textOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertModEvent appends one mod_events row inside tx.
func (s *Store) insertModEvent(ctx context.Context, tx queryer, action ModAction, target TargetType, targetID, reason string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO mod_events (action, target_type, target_id, reason, created_at) VALUES (?, ?, ?, ?, ?)`,
		string(action), string(target), targetID, reason, s.nowText())
	if err != nil {
		return fmt.Errorf("insert mod_event %s %s %s: %w", action, target, targetID, err)
	}
	return nil
}
