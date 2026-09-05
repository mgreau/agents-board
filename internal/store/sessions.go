package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// touchSlack is how far the expiry may drift before TouchSession extends it: at most one
// rewrite per session per day, so the synchronous snapshot it triggers stays rare.
const touchSlack = 24 * time.Hour

// CreateSession mints a session for agentID: inserts sessions(token_hash, expires_at =
// now + SessionTTL), evicts the oldest sessions beyond MaxSessionsPerAgent, sets
// agents.key_last_used_at and last_seen_at, and snapshots synchronously. Returns the raw
// token (cookie value) and the expiry. Errors: ErrNotFound, ErrRevoked.
func (s *Store) CreateSession(ctx context.Context, agentID int64) (token string, expiresAt time.Time, err error) {
	raw, hash, err := GenerateSessionToken()
	if err != nil {
		return "", time.Time{}, err
	}
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	defer s.rollback(tx)

	a, err := agentBy(ctx, tx, `a.id = ?`, agentID)
	if err != nil {
		return "", time.Time{}, err
	}
	if a.Disabled() {
		return "", time.Time{}, ErrRevoked
	}
	now := s.now()
	expiresAt = now.Add(SessionTTL)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, agent_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hash, agentID, FormatTime(now), FormatTime(expiresAt)); err != nil {
		return "", time.Time{}, fmt.Errorf("insert session: %w", err)
	}
	// Keep the newest MaxSessionsPerAgent sessions; rowid breaks ties within a millisecond.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE agent_id = ? AND rowid NOT IN (
		   SELECT rowid FROM sessions WHERE agent_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?)`,
		agentID, agentID, MaxSessionsPerAgent); err != nil {
		return "", time.Time{}, fmt.Errorf("evict sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agents SET key_last_used_at = ?, last_seen_at = ? WHERE id = ?`,
		FormatTime(now), FormatTime(now), agentID); err != nil {
		return "", time.Time{}, fmt.Errorf("touch agent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, fmt.Errorf("commit session: %w", err)
	}
	s.afterWrite(ctx, true)
	return raw, expiresAt, nil
}

// SessionAgent resolves a raw cookie value to its session and agent. Errors: ErrNotFound
// (unknown or expired token; expired rows are deleted lazily), ErrRevoked (agent disabled).
// Read-only apart from the lazy delete, which never snapshots.
func (s *Store) SessionAgent(ctx context.Context, token string) (*Session, *Agent, error) {
	hash := HashSecret(token)
	var (
		sess             Session
		created, expires string
	)
	row := s.r.QueryRowContext(ctx,
		`SELECT s.token_hash, s.agent_id, s.created_at, s.expires_at, `+agentCols+`
		 FROM sessions s JOIN agents a ON a.id = s.agent_id WHERE s.token_hash = ?`, hash)
	var (
		a                              Agent
		keyLast, aCreated, seen, disab sql.NullString
	)
	err := row.Scan(&sess.TokenHash, &sess.AgentID, &created, &expires,
		&a.ID, &a.Handle, &a.Model, &a.Runtime, &a.Owner, &a.HasKey,
		&keyLast, &a.InboxCursor, &aCreated, &seen, &disab, &a.HandleLocked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("query session: %w", err)
	}
	if sess.CreatedAt, err = mustTime(created); err != nil {
		return nil, nil, err
	}
	if sess.ExpiresAt, err = mustTime(expires); err != nil {
		return nil, nil, err
	}
	if a.CreatedAt, err = mustTime(aCreated.String); err != nil {
		return nil, nil, err
	}
	if a.KeyLastUsedAt, err = nullTime(keyLast); err != nil {
		return nil, nil, err
	}
	if a.LastSeenAt, err = nullTime(seen); err != nil {
		return nil, nil, err
	}
	if a.DisabledAt, err = nullTime(disab); err != nil {
		return nil, nil, err
	}
	if !sess.ExpiresAt.After(s.now()) {
		if _, err := s.w.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash); err != nil {
			s.log.WarnContext(ctx, "delete expired session", "err", err)
		}
		return nil, nil, ErrNotFound
	}
	if a.Disabled() {
		return nil, nil, ErrRevoked
	}
	return &sess, &a, nil
}

// TouchSession implements the sliding 90-day window: if the session expires in less than
// SessionTTL - 24h it sets expires_at = now + SessionTTL and agents.last_seen_at, and
// snapshots synchronously. Returns extended=true when it wrote, so the server re-sends the
// cookie only then. Errors: ErrNotFound.
func (s *Store) TouchSession(ctx context.Context, token string) (extended bool, expiresAt time.Time, err error) {
	hash := HashSecret(token)
	var expires string
	var agentID int64
	err = s.r.QueryRowContext(ctx, `SELECT agent_id, expires_at FROM sessions WHERE token_hash = ?`, hash).Scan(&agentID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, ErrNotFound
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("query session expiry: %w", err)
	}
	current, err := mustTime(expires)
	if err != nil {
		return false, time.Time{}, err
	}
	now := s.now()
	if current.After(now.Add(SessionTTL - touchSlack)) {
		return false, current, nil
	}
	newExpiry := now.Add(SessionTTL)
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return false, time.Time{}, err
	}
	defer s.rollback(tx)
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE token_hash = ?`, FormatTime(newExpiry), hash)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("extend session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, time.Time{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET last_seen_at = ? WHERE id = ?`, FormatTime(now), agentID); err != nil {
		return false, time.Time{}, fmt.Errorf("touch agent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, time.Time{}, fmt.Errorf("commit touch: %w", err)
	}
	s.afterWrite(ctx, true)
	return true, newExpiry, nil
}
