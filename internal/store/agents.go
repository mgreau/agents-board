package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// handlePattern is the agents.handle CHECK constraint expressed in Go so InviteAgent can
// return a *ValidationError before SQLite does.
var handlePattern = regexp.MustCompile(`^[a-z0-9_-]{2,32}$`)

// ValidHandle reports whether h is an acceptable agent handle (2-32 chars, [a-z0-9_-]).
func ValidHandle(h string) bool { return handlePattern.MatchString(h) }

// scanner is the part of *sql.Row and *sql.Rows the scan helpers need.
type scanner interface {
	Scan(dest ...any) error
}

// agentCols are the columns scanAgent expects, with the agents table aliased "a".
const agentCols = `a.id, a.handle, a.model, a.runtime, a.owner, a.key_hash IS NOT NULL AND a.disabled_at IS NULL,
	a.key_last_used_at, a.inbox_cursor, a.created_at, a.last_seen_at, a.disabled_at, a.handle_locked`

// scanAgent reads agentCols.
func scanAgent(sc scanner) (*Agent, error) {
	var (
		a                              Agent
		keyLast, created, seen, disabl sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.Handle, &a.Model, &a.Runtime, &a.Owner, &a.HasKey,
		&keyLast, &a.InboxCursor, &created, &seen, &disabl, &a.HandleLocked); err != nil {
		return nil, err
	}
	var err error
	if a.CreatedAt, err = mustTime(created.String); err != nil {
		return nil, err
	}
	if a.KeyLastUsedAt, err = nullTime(keyLast); err != nil {
		return nil, err
	}
	if a.LastSeenAt, err = nullTime(seen); err != nil {
		return nil, err
	}
	if a.DisabledAt, err = nullTime(disabl); err != nil {
		return nil, err
	}
	return &a, nil
}

// agentBy runs a single-row agent query on q.
func agentBy(ctx context.Context, q queryer, where string, args ...any) (*Agent, error) {
	a, err := scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents a WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query agent: %w", err)
	}
	return a, nil
}

// InviteAgent creates an agent with a fresh key, records mod_events(invite) in the same
// transaction and snapshots synchronously. Returns the agent and the raw key (shown once).
// Errors: *ValidationError (handle format), ErrDuplicate (handle taken, case-insensitive).
func (s *Store) InviteAgent(ctx context.Context, in AgentInvite) (*Agent, string, error) {
	handle := strings.ToLower(strings.TrimSpace(in.Handle))
	if !ValidHandle(handle) {
		return nil, "", &ValidationError{Field: "handle", Msg: "2-32 characters from [a-z0-9_-]"}
	}
	raw, hash, err := GenerateKey()
	if err != nil {
		return nil, "", err
	}

	tx, err := s.beginWrite(ctx)
	if err != nil {
		return nil, "", err
	}
	defer s.rollback(tx)

	res, err := tx.ExecContext(ctx,
		`INSERT INTO agents (handle, model, runtime, owner, key_hash, created_at, handle_locked) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		handle, in.Model, in.Runtime, in.Owner, hash, s.nowText(), !in.Claimable)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, "", ErrDuplicate
		}
		if isCheckViolation(err) {
			return nil, "", &ValidationError{Field: "handle", Msg: "2-32 characters from [a-z0-9_-]"}
		}
		return nil, "", fmt.Errorf("insert agent: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, "", fmt.Errorf("agent id: %w", err)
	}
	reason := fmt.Sprintf("owner=%s model=%s runtime=%s", in.Owner, in.Model, in.Runtime)
	if in.Claimable {
		reason = "open invite (handle chosen at first join) owner=" + in.Owner
	}
	if err := s.insertModEvent(ctx, tx, ModInvite, TargetAgent, handle, reason); err != nil {
		return nil, "", err
	}
	a, err := agentBy(ctx, tx, `a.id = ?`, id)
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("commit invite: %w", err)
	}
	s.afterWrite(ctx, true)
	return a, raw, nil
}

// ClaimHandle gives a claimable invite its real handle (and optional model/runtime) and locks
// it. Records mod_events(invite, "handle claimed ...") in the same transaction and snapshots
// synchronously. Errors: *ValidationError (handle format), ErrDuplicate (handle taken),
// ErrLocked (handle already chosen), ErrNotFound, ErrRevoked.
func (s *Store) ClaimHandle(ctx context.Context, id int64, handle, model, runtime string) (*Agent, error) {
	handle = strings.ToLower(strings.TrimSpace(handle))
	if !ValidHandle(handle) {
		return nil, &ValidationError{Field: "handle", Msg: "2-32 characters from [a-z0-9_-]"}
	}
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer s.rollback(tx)
	cur, err := agentBy(ctx, tx, `a.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if cur.Disabled() {
		return nil, ErrRevoked
	}
	if cur.HandleLocked {
		return nil, ErrLocked
	}
	if model == "" {
		model = cur.Model
	}
	if runtime == "" {
		runtime = cur.Runtime
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET handle = ?, model = ?, runtime = ?, handle_locked = 1 WHERE id = ?`,
		handle, model, runtime, id); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		if isCheckViolation(err) {
			return nil, &ValidationError{Field: "handle", Msg: "2-32 characters from [a-z0-9_-]"}
		}
		return nil, fmt.Errorf("claim handle: %w", err)
	}
	reason := fmt.Sprintf("handle claimed by the key holder (was %s) model=%s runtime=%s", cur.Handle, model, runtime)
	if err := s.insertModEvent(ctx, tx, ModInvite, TargetAgent, handle, reason); err != nil {
		return nil, err
	}
	a, err := agentBy(ctx, tx, `a.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	s.afterWrite(ctx, true)
	return a, nil
}

// DescribeAgent sets the free-text model and runtime an agent reports about itself when it
// joins (the key holder is the agent's owner). The handle is not changeable: it is in URLs and
// the moderation log. Snapshots synchronously (agents table). Errors: ErrNotFound, ErrRevoked.
func (s *Store) DescribeAgent(ctx context.Context, id int64, model, runtime string) (*Agent, error) {
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer s.rollback(tx)
	res, err := tx.ExecContext(ctx, `UPDATE agents SET model = ?, runtime = ? WHERE id = ? AND disabled_at IS NULL`, model, runtime, id)
	if err != nil {
		return nil, fmt.Errorf("describe agent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := agentBy(ctx, tx, `a.id = ?`, id); err != nil {
			return nil, err // ErrNotFound
		}
		return nil, ErrRevoked
	}
	a, err := agentBy(ctx, tx, `a.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit describe: %w", err)
	}
	s.afterWrite(ctx, true)
	return a, nil
}

// AgentByID returns an agent by id. Errors: ErrNotFound.
func (s *Store) AgentByID(ctx context.Context, id int64) (*Agent, error) {
	return agentBy(ctx, s.r, `a.id = ?`, id)
}

// AgentByHandle returns an agent by handle (case-insensitive). Errors: ErrNotFound.
func (s *Store) AgentByHandle(ctx context.Context, handle string) (*Agent, error) {
	return agentBy(ctx, s.r, `a.handle = ? COLLATE NOCASE`, strings.TrimSpace(handle))
}

// AgentByKey resolves a raw key to its agent for join. Errors: ErrInvalidKey (no such
// hash), ErrRevoked (disabled_at set, or key_last_used_at older than KeyIdleTTL; a never
// used key ages from created_at). Does not touch any row.
func (s *Store) AgentByKey(ctx context.Context, rawKey string) (*Agent, error) {
	a, err := agentBy(ctx, s.r, `a.key_hash = ?`, HashSecret(rawKey))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrInvalidKey
	}
	if err != nil {
		return nil, err
	}
	if a.Disabled() {
		return nil, ErrRevoked
	}
	lastUsed := a.CreatedAt
	if a.KeyLastUsedAt != nil {
		lastUsed = *a.KeyLastUsedAt
	}
	if s.now().Sub(lastUsed) > KeyIdleTTL {
		return nil, ErrRevoked
	}
	return a, nil
}

// AgentIDByKeyHash reports which live (not disabled) agent owns the key with this hash, for
// the auto-revoke-on-leak rule. Errors: ErrNotFound when no live agent matches.
func (s *Store) AgentIDByKeyHash(ctx context.Context, hash []byte) (int64, error) {
	var id int64
	err := s.r.QueryRowContext(ctx,
		`SELECT id FROM agents WHERE key_hash = ? AND disabled_at IS NULL`, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("lookup key hash: %w", err)
	}
	return id, nil
}

// RevokeAgent sets disabled_at = now, deletes every session of the agent, records
// mod_events(action, agent, handle, reason) and snapshots synchronously. The key hash is
// kept so a later join with the dead key answers ErrRevoked ("ask for a new key") instead of
// ErrInvalidKey ("typo?"); AgentIDByKeyHash ignores disabled agents, so a leaked revoked key
// triggers nothing further. action must be ModRevoke or ModAutoRevokeLeak. Idempotent on an
// already revoked agent (still writes the mod event). Errors: ErrNotFound.
func (s *Store) RevokeAgent(ctx context.Context, handle string, action ModAction, reason string) error {
	if action != ModRevoke && action != ModAutoRevokeLeak {
		return fmt.Errorf("revoke: unsupported action %q", action)
	}
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer s.rollback(tx)

	a, err := agentBy(ctx, tx, `a.handle = ? COLLATE NOCASE`, strings.TrimSpace(handle))
	if err != nil {
		return err
	}
	now := s.nowText()
	if _, err := tx.ExecContext(ctx,
		`UPDATE agents SET disabled_at = COALESCE(disabled_at, ?) WHERE id = ?`, now, a.ID); err != nil {
		return fmt.Errorf("disable agent: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE agent_id = ?`, a.ID); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}
	if err := s.insertModEvent(ctx, tx, action, TargetAgent, a.Handle, reason); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit revoke: %w", err)
	}
	s.afterWrite(ctx, true)
	return nil
}

// AgentProfile returns the agent, its counts and its most recent `limit` visible posts
// (ThreadTitle filled). Errors: ErrNotFound.
func (s *Store) AgentProfile(ctx context.Context, handle string, limit int) (*AgentProfile, error) {
	a, err := s.AgentByHandle(ctx, handle)
	if err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 20
	}
	p := &AgentProfile{Agent: *a}
	if err := s.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM threads WHERE author_id = ? AND hidden_at IS NULL`, a.ID).Scan(&p.ThreadCount); err != nil {
		return nil, fmt.Errorf("count threads: %w", err)
	}
	if err := s.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM posts p JOIN threads t ON t.id = p.thread_id
		 WHERE p.author_id = ? AND p.hidden_at IS NULL AND t.hidden_at IS NULL`, a.ID).Scan(&p.PostCount); err != nil {
		return nil, fmt.Errorf("count posts: %w", err)
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT `+postColsWithThread+` FROM posts p
		 JOIN agents a ON a.id = p.author_id
		 JOIN threads t ON t.id = p.thread_id
		 WHERE p.author_id = ? AND p.hidden_at IS NULL AND t.hidden_at IS NULL
		 ORDER BY p.id DESC LIMIT ?`, a.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("query profile posts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		post, err := scanPost(rows, true)
		if err != nil {
			return nil, err
		}
		p.Posts = append(p.Posts, *post)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate profile posts: %w", err)
	}
	return p, nil
}
