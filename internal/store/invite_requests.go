package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const inviteRequestCols = `id, handle_wanted, model, runtime, owner, contact, note, status, agent_id, decision_note, decided_at, created_at`

func scanInviteRequest(sc scanner) (*InviteRequest, error) {
	var (
		r                InviteRequest
		status           string
		agentID          sql.NullInt64
		decided, created sql.NullString
	)
	if err := sc.Scan(&r.ID, &r.HandleWanted, &r.Model, &r.Runtime, &r.Owner, &r.Contact, &r.Note, &status,
		&agentID, &r.DecisionNote, &decided, &created); err != nil {
		return nil, err
	}
	r.Status = InviteRequestStatus(status)
	if agentID.Valid {
		id := agentID.Int64
		r.AgentID = &id
	}
	var err error
	if r.CreatedAt, err = mustTime(created.String); err != nil {
		return nil, err
	}
	if r.DecidedAt, err = nullTime(decided); err != nil {
		return nil, err
	}
	return &r, nil
}

// InviteRequestLoad is what CreateInviteRequest's caller needs for rate limiting: requests
// from one address and from everyone in the last 24 h, with the oldest timestamps so a
// Retry-After can be computed.
type InviteRequestLoad struct {
	FromIP       int
	OldestFromIP *time.Time
	Total        int
	OldestTotal  *time.Time
}

// InviteRequestLoad counts requests since `since`, for ipHash (nil = skip) and overall.
func (s *Store) InviteRequestLoad(ctx context.Context, ipHash []byte, since time.Time) (InviteRequestLoad, error) {
	var out InviteRequestLoad
	var oldest sql.NullString
	if err := s.r.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(created_at) FROM invite_requests WHERE created_at >= ?`, FormatTime(since),
	).Scan(&out.Total, &oldest); err != nil {
		return out, fmt.Errorf("invite request load: %w", err)
	}
	var err error
	if out.OldestTotal, err = nullTime(oldest); err != nil {
		return out, err
	}
	if ipHash != nil {
		if err := s.r.QueryRowContext(ctx,
			`SELECT COUNT(*), MIN(created_at) FROM invite_requests WHERE ip_hash = ? AND created_at >= ?`, ipHash, FormatTime(since),
		).Scan(&out.FromIP, &oldest); err != nil {
			return out, fmt.Errorf("invite request load by ip: %w", err)
		}
		if out.OldestFromIP, err = nullTime(oldest); err != nil {
			return out, err
		}
	}
	return out, nil
}

// CreateInviteRequest stores a pending request. A pending request with the same contact
// (case-insensitive) is ErrDuplicate. Snapshots synchronously: this is the only record of
// the request.
func (s *Store) CreateInviteRequest(ctx context.Context, in NewInviteRequest) (*InviteRequest, error) {
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer s.rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM invite_requests WHERE status = 'pending' AND lower(contact) = lower(?)`, in.Contact,
	).Scan(&n); err != nil {
		return nil, fmt.Errorf("pending by contact: %w", err)
	}
	if n > 0 {
		return nil, ErrDuplicate
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO invite_requests (handle_wanted, model, runtime, owner, contact, note, ip_hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.ToLower(in.HandleWanted), in.Model, in.Runtime, in.Owner, in.Contact, in.Note, in.IPHash, s.nowText())
	if err != nil {
		return nil, fmt.Errorf("insert invite request: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("invite request id: %w", err)
	}
	r, err := scanInviteRequest(tx.QueryRowContext(ctx, `SELECT `+inviteRequestCols+` FROM invite_requests WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("read invite request: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit invite request: %w", err)
	}
	s.afterWrite(ctx, true)
	return r, nil
}

// InviteRequestByID returns one request. Errors: ErrNotFound.
func (s *Store) InviteRequestByID(ctx context.Context, id int64) (*InviteRequest, error) {
	r, err := scanInviteRequest(s.r.QueryRowContext(ctx, `SELECT `+inviteRequestCols+` FROM invite_requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("invite request by id: %w", err)
	}
	return r, nil
}

// ListInviteRequests returns up to limit requests with the given status, oldest first.
func (s *Store) ListInviteRequests(ctx context.Context, status InviteRequestStatus, limit int) ([]InviteRequest, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT `+inviteRequestCols+` FROM invite_requests WHERE status = ? ORDER BY created_at ASC, id ASC LIMIT ?`, string(status), limit)
	if err != nil {
		return nil, fmt.Errorf("list invite requests: %w", err)
	}
	defer rows.Close()
	var out []InviteRequest
	for rows.Next() {
		r, err := scanInviteRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// DecideInviteRequest marks a pending request approved (with the minted agent) or denied.
// Errors: ErrNotFound, ErrLocked when the request was already decided.
func (s *Store) DecideInviteRequest(ctx context.Context, id int64, status InviteRequestStatus, agentID int64, note string) (*InviteRequest, error) {
	if status != InviteRequestApproved && status != InviteRequestDenied {
		return nil, fmt.Errorf("decide invite request: bad status %q", status)
	}
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer s.rollback(tx)
	var agent any
	if agentID > 0 {
		agent = agentID
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE invite_requests SET status = ?, agent_id = ?, decision_note = ?, decided_at = ? WHERE id = ? AND status = 'pending'`,
		string(status), agent, note, s.nowText(), id)
	if err != nil {
		return nil, fmt.Errorf("decide invite request: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := scanInviteRequest(tx.QueryRowContext(ctx, `SELECT `+inviteRequestCols+` FROM invite_requests WHERE id = ?`, id)); errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, ErrLocked
	}
	r, err := scanInviteRequest(tx.QueryRowContext(ctx, `SELECT `+inviteRequestCols+` FROM invite_requests WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("read invite request: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit decision: %w", err)
	}
	s.afterWrite(ctx, true)
	return r, nil
}
