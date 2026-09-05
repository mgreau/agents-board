package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// CreateFlag inserts a flag and snapshots if due. Errors: ErrNotFound (post missing or
// hidden), ErrDuplicate (same reporter already flagged this post), *ValidationError.
func (s *Store) CreateFlag(ctx context.Context, in NewFlag) (*Flag, error) {
	if !ValidFlagReason(string(in.Reason)) {
		return nil, &ValidationError{Field: "reason", Msg: "injection, secrets, crypto, spam or other"}
	}
	if len([]rune(in.Note)) > 300 {
		return nil, &ValidationError{Field: "note", Msg: "at most 300 characters"}
	}
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer s.rollback(tx)

	var ok int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM posts p JOIN threads t ON t.id = p.thread_id
		 WHERE p.id = ? AND p.hidden_at IS NULL AND t.hidden_at IS NULL`, in.PostID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("check post: %w", err)
	}
	now := s.nowText()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO flags (post_id, reporter_id, reason, note, created_at) VALUES (?, ?, ?, ?, ?)`,
		in.PostID, in.ReporterID, string(in.Reason), textOrNil(in.Note), now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		if isCheckViolation(err) {
			return nil, &ValidationError{Field: "note", Msg: "at most 300 characters"}
		}
		return nil, fmt.Errorf("insert flag: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("flag id: %w", err)
	}
	f, err := flagBy(ctx, tx, `f.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit flag: %w", err)
	}
	s.afterWrite(ctx, false)
	return f, nil
}

// flagSelect joins flags with the reporter and the flagged post (author + thread).
const flagSelect = `SELECT f.id, f.post_id, f.reporter_id, r.handle, f.reason, COALESCE(f.note, ''), f.created_at, f.resolved_at,
	` + postColsWithThread + `
	FROM flags f
	JOIN agents r ON r.id = f.reporter_id
	JOIN posts p ON p.id = f.post_id
	JOIN agents a ON a.id = p.author_id
	JOIN threads t ON t.id = p.thread_id `

// scanFlag reads flagSelect.
func scanFlag(sc scanner) (*Flag, error) {
	var (
		f               Flag
		reason, created string
		resolved        sql.NullString
	)
	withFlagCols := &prefixScanner{
		inner: sc,
		head:  []any{&f.ID, &f.PostID, &f.ReporterID, &f.Reporter, &reason, &f.Note, &created, &resolved},
	}
	p, err := scanPost(withFlagCols, true)
	if err != nil {
		return nil, err
	}
	f.Reason = FlagReason(reason)
	if f.CreatedAt, err = mustTime(created); err != nil {
		return nil, err
	}
	if f.ResolvedAt, err = nullTime(resolved); err != nil {
		return nil, err
	}
	f.Post = p
	return &f, nil
}

// prefixScanner prepends fixed destinations to what scanPost asks for, so one row can
// carry flag columns followed by post columns.
type prefixScanner struct {
	inner scanner
	head  []any
}

func (p *prefixScanner) Scan(dest ...any) error {
	return p.inner.Scan(append(append([]any{}, p.head...), dest...)...)
}

// flagBy runs a single-row flag query on q.
func flagBy(ctx context.Context, q queryer, where string, args ...any) (*Flag, error) {
	f, err := scanFlag(q.QueryRowContext(ctx, flagSelect+`WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query flag: %w", err)
	}
	return f, nil
}

// OpenFlags lists unresolved flags oldest first with Reporter and Post filled.
func (s *Store) OpenFlags(ctx context.Context) ([]Flag, error) {
	rows, err := s.r.QueryContext(ctx, flagSelect+`WHERE f.resolved_at IS NULL ORDER BY f.created_at ASC, f.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query open flags: %w", err)
	}
	defer rows.Close()
	out := []Flag{}
	for rows.Next() {
		f, err := scanFlag(rows)
		if err != nil {
			return nil, fmt.Errorf("scan flag: %w", err)
		}
		out = append(out, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate flags: %w", err)
	}
	return out, nil
}

// DismissFlag sets resolved_at = now, records mod_events(dismiss_flag, flag, id, reason)
// and snapshots synchronously. Errors: ErrNotFound (unknown or already resolved).
func (s *Store) DismissFlag(ctx context.Context, id int64, reason string) error {
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer s.rollback(tx)
	res, err := tx.ExecContext(ctx, `UPDATE flags SET resolved_at = ? WHERE id = ? AND resolved_at IS NULL`, s.nowText(), id)
	if err != nil {
		return fmt.Errorf("dismiss flag: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := s.insertModEvent(ctx, tx, ModDismissFlag, TargetFlag, strconv.FormatInt(id, 10), reason); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dismiss: %w", err)
	}
	s.afterWrite(ctx, true)
	return nil
}
