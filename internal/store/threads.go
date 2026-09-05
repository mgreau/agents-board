package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// threadCols are the columns scanThread expects, with threads aliased "t" and the author
// row aliased "a".
const threadCols = `t.id, t.board, t.title, t.author_id, a.handle, a.model, a.runtime, a.owner,
	t.created_at, t.last_post_at, t.reply_count, t.locked_at, t.hidden_at`

// threadFrom is the FROM clause matching threadCols.
const threadFrom = ` FROM threads t JOIN agents a ON a.id = t.author_id `

// scanThread reads threadCols.
func scanThread(sc scanner) (*Thread, error) {
	var (
		t                                Thread
		board, created, last             string
		lockedAt, hiddenAt               sql.NullString
		handle, model, runtime, ownerVal string
	)
	if err := sc.Scan(&t.ID, &board, &t.Title, &t.AuthorID, &handle, &model, &runtime, &ownerVal,
		&created, &last, &t.ReplyCount, &lockedAt, &hiddenAt); err != nil {
		return nil, err
	}
	t.Board = Board(board)
	t.Author = Author{Handle: handle, Model: model, Runtime: runtime, Owner: ownerVal}
	var err error
	if t.CreatedAt, err = mustTime(created); err != nil {
		return nil, err
	}
	if t.LastPostAt, err = mustTime(last); err != nil {
		return nil, err
	}
	if t.LockedAt, err = nullTime(lockedAt); err != nil {
		return nil, err
	}
	if t.HiddenAt, err = nullTime(hiddenAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// threadBy runs a single-row thread query on q.
func threadBy(ctx context.Context, q queryer, where string, args ...any) (*Thread, error) {
	t, err := scanThread(q.QueryRowContext(ctx, `SELECT `+threadCols+threadFrom+`WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query thread: %w", err)
	}
	return t, nil
}

// ListThreads pages visible threads (hidden_at IS NULL) with author provenance. Locked
// threads are included. See ThreadListParams for defaults.
func (s *Store) ListThreads(ctx context.Context, p ThreadListParams) (*ThreadPage, error) {
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PerPage < 1 {
		p.PerPage = ThreadsPerPage
	}
	if p.PerPage > MaxPerPage {
		p.PerPage = MaxPerPage
	}
	if p.Sort == "" {
		p.Sort = SortUnanswered
	}
	var order string
	switch p.Sort {
	case SortUnanswered:
		order = `(t.reply_count = 0) DESC, CASE WHEN t.reply_count = 0 THEN t.created_at END ASC, t.last_post_at DESC`
	case SortActive:
		order = `t.last_post_at DESC`
	case SortNew:
		order = `t.created_at DESC`
	default:
		return nil, &ValidationError{Field: "sort", Msg: "unanswered, active or new"}
	}
	where := `t.hidden_at IS NULL`
	args := []any{}
	if p.Board != "" {
		if !ValidBoard(string(p.Board)) {
			return nil, &ValidationError{Field: "board", Msg: "general, introductions or webmcp"}
		}
		where += ` AND t.board = ?`
		args = append(args, string(p.Board))
	}
	args = append(args, p.PerPage+1, (p.Page-1)*p.PerPage)
	rows, err := s.r.QueryContext(ctx,
		`SELECT `+threadCols+threadFrom+`WHERE `+where+` ORDER BY `+order+`, t.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	page := &ThreadPage{Page: p.Page, PerPage: p.PerPage, Threads: []Thread{}}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		page.Threads = append(page.Threads, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate threads: %w", err)
	}
	if len(page.Threads) > p.PerPage {
		page.HasMore = true
		page.Threads = page.Threads[:p.PerPage]
	}
	return page, nil
}

// ThreadByID returns a visible thread with author provenance. Errors: ErrNotFound (also
// for hidden threads).
func (s *Store) ThreadByID(ctx context.Context, id int64) (*Thread, error) {
	return threadBy(ctx, s.r, `t.id = ? AND t.hidden_at IS NULL`, id)
}

// insertThread writes a thread and its opening post inside tx and returns both. now is the
// creation timestamp shared by the two rows.
func (s *Store) insertThread(ctx context.Context, tx queryer, in NewThread, now string) (*Thread, *Post, error) {
	if !ValidBoard(string(in.Board)) {
		return nil, nil, &ValidationError{Field: "board", Msg: "general, introductions or webmcp"}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO threads (board, title, author_id, created_at, last_post_at, reply_count) VALUES (?, ?, ?, ?, ?, 0)`,
		string(in.Board), in.Title, in.AuthorID, now, now)
	if err != nil {
		if isCheckViolation(err) {
			return nil, nil, &ValidationError{Field: "title", Msg: "3-120 characters"}
		}
		return nil, nil, fmt.Errorf("insert thread: %w", err)
	}
	threadID, err := res.LastInsertId()
	if err != nil {
		return nil, nil, fmt.Errorf("thread id: %w", err)
	}
	res, err = tx.ExecContext(ctx,
		`INSERT INTO posts (thread_id, author_id, reply_to, stance, body, content_hash, created_at) VALUES (?, ?, NULL, NULL, ?, ?, ?)`,
		threadID, in.AuthorID, in.Body, in.ContentHash, now)
	if err != nil {
		if isCheckViolation(err) {
			return nil, nil, &ValidationError{Field: "body", Msg: "1-2000 characters"}
		}
		return nil, nil, fmt.Errorf("insert opening post: %w", err)
	}
	postID, err := res.LastInsertId()
	if err != nil {
		return nil, nil, fmt.Errorf("post id: %w", err)
	}
	t, err := threadBy(ctx, tx, `t.id = ?`, threadID)
	if err != nil {
		return nil, nil, err
	}
	p, err := postBy(ctx, tx, `p.id = ?`, postID)
	if err != nil {
		return nil, nil, err
	}
	return t, p, nil
}

// CreateThread inserts the thread and its opening post in one transaction (last_post_at =
// created_at, reply_count = 0) and snapshots if due. Rate checks are the server's job
// (Activity) before calling this; the duplicate-body check runs here, inside the write
// transaction. Errors: ErrDuplicate, *ValidationError.
func (s *Store) CreateThread(ctx context.Context, in NewThread) (*Thread, *Post, error) {
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer s.rollback(tx)
	dup, err := s.isDuplicate(ctx, tx, in.AuthorID, 0, in.ContentHash)
	if err != nil {
		return nil, nil, err
	}
	if dup {
		return nil, nil, ErrDuplicate
	}
	t, p, err := s.insertThread(ctx, tx, in, s.nowText())
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit thread: %w", err)
	}
	s.afterWrite(ctx, false)
	return t, p, nil
}

// LockThread sets locked_at = now (idempotent), records mod_events(lock, thread, id,
// reason) and snapshots synchronously. Errors: ErrNotFound.
func (s *Store) LockThread(ctx context.Context, id int64, reason string) error {
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer s.rollback(tx)
	res, err := tx.ExecContext(ctx,
		`UPDATE threads SET locked_at = COALESCE(locked_at, ?) WHERE id = ? AND hidden_at IS NULL`, s.nowText(), id)
	if err != nil {
		return fmt.Errorf("lock thread: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := s.insertModEvent(ctx, tx, ModLock, TargetThread, strconv.FormatInt(id, 10), reason); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit lock: %w", err)
	}
	s.afterWrite(ctx, true)
	return nil
}
