package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Sliding-window counters. None of these call afterWrite: they are bookkeeping, and losing
// them on a crash costs nothing.

// RecordRate appends a rate_events row. Never triggers a snapshot.
func (s *Store) RecordRate(ctx context.Context, kind RateKind, key []byte) error {
	if _, err := s.w.ExecContext(ctx,
		`INSERT INTO rate_events (kind, key, created_at) VALUES (?, ?, ?)`, string(kind), key, s.nowText()); err != nil {
		return fmt.Errorf("record rate %s: %w", kind, err)
	}
	return nil
}

// CountRate counts rate_events rows for (kind, key) created after since.
func (s *Store) CountRate(ctx context.Context, kind RateKind, key []byte, since time.Time) (int, error) {
	n, _, err := s.RateWindow(ctx, kind, key, since)
	return n, err
}

// RateWindow counts rate_events rows for (kind, key) created after since and returns the
// oldest one in the window (nil when the window is empty), which is what a 429 needs to
// compute retry_after_s.
func (s *Store) RateWindow(ctx context.Context, kind RateKind, key []byte, since time.Time) (int, *time.Time, error) {
	var n int
	var oldest sql.NullString
	err := s.r.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(created_at) FROM rate_events WHERE kind = ? AND key = ? AND created_at > ?`,
		string(kind), key, FormatTime(since)).Scan(&n, &oldest)
	if err != nil {
		return 0, nil, fmt.Errorf("count rate %s: %w", kind, err)
	}
	t, err := nullTime(oldest)
	if err != nil {
		return 0, nil, err
	}
	return n, t, nil
}

// PruneRateEvents deletes rate_events older than before. The server calls it opportunistically
// (for example once per minute from the request path). Never triggers a snapshot.
func (s *Store) PruneRateEvents(ctx context.Context, before time.Time) error {
	if _, err := s.w.ExecContext(ctx, `DELETE FROM rate_events WHERE created_at < ?`, FormatTime(before)); err != nil {
		return fmt.Errorf("prune rate_events: %w", err)
	}
	return nil
}

// RecordStat appends a stat_events row (dup_rejected, gate_refused, rate_limited,
// content_blocked). Never triggers a snapshot.
func (s *Store) RecordStat(ctx context.Context, kind StatKind) error {
	if _, err := s.w.ExecContext(ctx,
		`INSERT INTO stat_events (kind, created_at) VALUES (?, ?)`, string(kind), s.nowText()); err != nil {
		return fmt.Errorf("record stat %s: %w", kind, err)
	}
	return nil
}
