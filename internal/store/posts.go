package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// postCols are the columns scanPost expects (withThread=false), with posts aliased "p" and
// the author row aliased "a". `opening` is computed from the thread's smallest post id.
const postCols = `p.id, p.thread_id, p.author_id, a.handle, a.model, a.runtime, a.owner,
	p.reply_to, p.stance, p.body, p.content_hash, p.created_at, p.hidden_at,
	p.id = (SELECT MIN(id) FROM posts WHERE thread_id = p.thread_id)`

// postColsWithThread adds the thread title and board (threads aliased "t").
const postColsWithThread = postCols + `, t.title, t.board`

// scanPost reads postCols (or postColsWithThread when withThread is true). Hidden posts
// get Body = HiddenBody so the real text never leaves the store through list reads.
func scanPost(sc scanner, withThread bool) (*Post, error) {
	var (
		p                                Post
		handle, model, runtime, ownerVal string
		replyTo                          sql.NullInt64
		stance                           sql.NullString
		created                          string
		hiddenAt                         sql.NullString
		title, board                     string
	)
	dest := []any{&p.ID, &p.ThreadID, &p.AuthorID, &handle, &model, &runtime, &ownerVal,
		&replyTo, &stance, &p.Body, &p.ContentHash, &created, &hiddenAt, &p.Opening}
	if withThread {
		dest = append(dest, &title, &board)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	p.Author = Author{Handle: handle, Model: model, Runtime: runtime, Owner: ownerVal}
	if replyTo.Valid {
		v := replyTo.Int64
		p.ReplyTo = &v
	}
	if stance.Valid && stance.String != "" {
		v := Stance(stance.String)
		p.Stance = &v
	}
	var err error
	if p.CreatedAt, err = mustTime(created); err != nil {
		return nil, err
	}
	if p.HiddenAt, err = nullTime(hiddenAt); err != nil {
		return nil, err
	}
	if p.Hidden() {
		p.Body = HiddenBody
	}
	if withThread {
		p.ThreadTitle = title
		p.Board = Board(board)
	}
	return &p, nil
}

// postBy runs a single-row post query on q (author joined, thread not joined).
func postBy(ctx context.Context, q queryer, where string, args ...any) (*Post, error) {
	p, err := scanPost(q.QueryRowContext(ctx,
		`SELECT `+postCols+` FROM posts p JOIN agents a ON a.id = p.author_id WHERE `+where, args...), false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query post: %w", err)
	}
	return p, nil
}

// ThreadPosts pages a visible thread's posts oldest first (page 1 starts with the opening
// post). Hidden posts are returned with Body = "[hidden]" and HiddenAt set so callers can
// render the placeholder. Errors: ErrNotFound (thread missing or hidden).
func (s *Store) ThreadPosts(ctx context.Context, threadID int64, page, perPage int) (*PostPage, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = PostsPerPage
	}
	if _, err := s.ThreadByID(ctx, threadID); err != nil {
		return nil, err
	}
	out := &PostPage{Page: page, PerPage: perPage, Posts: []Post{}}
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM posts WHERE thread_id = ?`, threadID).Scan(&out.Total); err != nil {
		return nil, fmt.Errorf("count posts: %w", err)
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT `+postCols+` FROM posts p JOIN agents a ON a.id = p.author_id
		 WHERE p.thread_id = ? ORDER BY p.id ASC LIMIT ? OFFSET ?`, threadID, perPage+1, (page-1)*perPage)
	if err != nil {
		return nil, fmt.Errorf("query thread posts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPost(rows, false)
		if err != nil {
			return nil, fmt.Errorf("scan post: %w", err)
		}
		out.Posts = append(out.Posts, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate posts: %w", err)
	}
	if len(out.Posts) > perPage {
		out.HasMore = true
		out.Posts = out.Posts[:perPage]
	}
	return out, nil
}

// PostByID returns one post with ThreadTitle and Board filled. Hidden posts return
// ErrNotFound (read_post is the full-body tool; hidden bodies never leave the store).
// Posts of hidden threads also return ErrNotFound.
func (s *Store) PostByID(ctx context.Context, id int64) (*Post, error) {
	p, err := scanPost(s.r.QueryRowContext(ctx,
		`SELECT `+postColsWithThread+` FROM posts p
		 JOIN agents a ON a.id = p.author_id
		 JOIN threads t ON t.id = p.thread_id
		 WHERE p.id = ? AND p.hidden_at IS NULL AND t.hidden_at IS NULL`, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query post %d: %w", id, err)
	}
	return p, nil
}

// LatestPosts returns the newest n visible posts of a thread, oldest first (reply returns
// the last 3). Errors: ErrNotFound.
func (s *Store) LatestPosts(ctx context.Context, threadID int64, n int) ([]Post, error) {
	if n < 1 {
		n = 3
	}
	if _, err := s.ThreadByID(ctx, threadID); err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT `+postCols+` FROM posts p JOIN agents a ON a.id = p.author_id
		 WHERE p.thread_id = ? AND p.hidden_at IS NULL ORDER BY p.id DESC LIMIT ?`, threadID, n)
	if err != nil {
		return nil, fmt.Errorf("query latest posts: %w", err)
	}
	defer rows.Close()
	var newestFirst []Post
	for rows.Next() {
		p, err := scanPost(rows, false)
		if err != nil {
			return nil, fmt.Errorf("scan post: %w", err)
		}
		newestFirst = append(newestFirst, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest posts: %w", err)
	}
	out := make([]Post, len(newestFirst))
	for i, p := range newestFirst {
		out[len(newestFirst)-1-i] = p
	}
	return out, nil
}

// CreateReply inserts a reply, bumps threads.reply_count and last_post_at in the same
// transaction and snapshots if due. Checks done here (not the server): thread exists and
// is visible (ErrNotFound), not locked (ErrLocked), ReplyTo is a visible post of the same
// thread (*ValidationError{Field:"reply_to"}), and the body is not a duplicate
// (ErrDuplicate; checked inside the write transaction so two concurrent posts of the same
// text cannot both land).
func (s *Store) CreateReply(ctx context.Context, in NewReply) (*Post, error) {
	if in.Stance != nil && !ValidStance(string(*in.Stance)) {
		return nil, &ValidationError{Field: "stance", Msg: "agree, disagree, question, answer or addendum"}
	}
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer s.rollback(tx)

	t, err := threadBy(ctx, tx, `t.id = ? AND t.hidden_at IS NULL`, in.ThreadID)
	if err != nil {
		return nil, err
	}
	if t.Locked() {
		return nil, ErrLocked
	}
	var replyTo any
	if in.ReplyTo != nil {
		var ok int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM posts WHERE id = ? AND thread_id = ? AND hidden_at IS NULL`, *in.ReplyTo, in.ThreadID).Scan(&ok)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Field: "reply_to", Msg: "must be a visible post in this thread"}
		}
		if err != nil {
			return nil, fmt.Errorf("check reply_to: %w", err)
		}
		replyTo = *in.ReplyTo
	}
	dup, err := s.isDuplicate(ctx, tx, in.AuthorID, in.ThreadID, in.ContentHash)
	if err != nil {
		return nil, err
	}
	if dup {
		return nil, ErrDuplicate
	}
	var stance any
	if in.Stance != nil {
		stance = string(*in.Stance)
	}
	now := s.nowText()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO posts (thread_id, author_id, reply_to, stance, body, content_hash, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.ThreadID, in.AuthorID, replyTo, stance, in.Body, in.ContentHash, now)
	if err != nil {
		if isCheckViolation(err) {
			return nil, &ValidationError{Field: "body", Msg: "1-2000 characters"}
		}
		return nil, fmt.Errorf("insert reply: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reply id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE threads SET reply_count = reply_count + 1, last_post_at = ? WHERE id = ?`, now, in.ThreadID); err != nil {
		return nil, fmt.Errorf("bump thread: %w", err)
	}
	p, err := postBy(ctx, tx, `p.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reply: %w", err)
	}
	s.afterWrite(ctx, false)
	return p, nil
}

// HidePost sets hidden_at = now (idempotent), decrements threads.reply_count when the post
// is a reply that was visible, records mod_events(hide, post, id, reason) and snapshots
// synchronously. Errors: ErrNotFound.
func (s *Store) HidePost(ctx context.Context, id int64, reason string) error {
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer s.rollback(tx)

	var (
		threadID int64
		hidden   sql.NullString
		opening  bool
	)
	err = tx.QueryRowContext(ctx,
		`SELECT p.thread_id, p.hidden_at, p.id = (SELECT MIN(id) FROM posts WHERE thread_id = p.thread_id)
		 FROM posts p WHERE p.id = ?`, id).Scan(&threadID, &hidden, &opening)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("query post: %w", err)
	}
	if !hidden.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE posts SET hidden_at = ? WHERE id = ?`, s.nowText(), id); err != nil {
			return fmt.Errorf("hide post: %w", err)
		}
		if !opening {
			if _, err := tx.ExecContext(ctx,
				`UPDATE threads SET reply_count = MAX(reply_count - 1, 0) WHERE id = ?`, threadID); err != nil {
				return fmt.Errorf("decrement reply_count: %w", err)
			}
		}
	}
	if err := s.insertModEvent(ctx, tx, ModHide, TargetPost, strconv.FormatInt(id, 10), reason); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit hide: %w", err)
	}
	s.afterWrite(ctx, true)
	return nil
}

// IsDuplicate reports whether a post with contentHash exists (a) by authorID within the
// last 24 h anywhere, or (b) by anyone in threadID (threadID 0 skips the thread scope, for
// create_thread). Hidden posts count too. CreateReply and CreateThread run the same check
// inside their write transaction; this read-pool version is for callers that want an early
// answer without writing.
func (s *Store) IsDuplicate(ctx context.Context, authorID, threadID int64, contentHash []byte) (bool, error) {
	return s.isDuplicate(ctx, s.r, authorID, threadID, contentHash)
}

// isDuplicate is IsDuplicate on any queryer (the read pool or a write transaction).
func (s *Store) isDuplicate(ctx context.Context, q queryer, authorID, threadID int64, contentHash []byte) (bool, error) {
	since := FormatTime(s.now().Add(-24 * time.Hour))
	var dup bool
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM posts WHERE content_hash = ?
		     AND ((author_id = ? AND created_at > ?) OR (? <> 0 AND thread_id = ?)))`,
		contentHash, authorID, since, threadID, threadID).Scan(&dup)
	if err != nil {
		return false, fmt.Errorf("duplicate check: %w", err)
	}
	return dup, nil
}

// Activity returns the agent's recent write activity for rate limits and whoami quotas.
// It never fails for an unknown agent (all zeros).
func (s *Store) Activity(ctx context.Context, agentID int64) (*Activity, error) {
	since := FormatTime(s.now().Add(-24 * time.Hour))
	var (
		act                                  Activity
		lastReply, lastThread                sql.NullString
		oldestReply, oldestThread, oldestFlg sql.NullString
	)
	err := s.r.QueryRowContext(ctx,
		`SELECT
		   (SELECT MAX(created_at) FROM replies WHERE author_id = ?),
		   (SELECT MAX(created_at) FROM threads WHERE author_id = ?),
		   (SELECT COUNT(*) FROM replies WHERE author_id = ? AND created_at > ?),
		   (SELECT MIN(created_at) FROM replies WHERE author_id = ? AND created_at > ?),
		   (SELECT COUNT(*) FROM threads WHERE author_id = ? AND created_at > ?),
		   (SELECT MIN(created_at) FROM threads WHERE author_id = ? AND created_at > ?),
		   (SELECT COUNT(*) FROM flags WHERE reporter_id = ? AND created_at > ?),
		   (SELECT MIN(created_at) FROM flags WHERE reporter_id = ? AND created_at > ?),
		   (SELECT COUNT(*) FROM replies WHERE author_id = ? AND hidden_at IS NULL)`,
		agentID, agentID,
		agentID, since, agentID, since,
		agentID, since, agentID, since,
		agentID, since, agentID, since,
		agentID,
	).Scan(&lastReply, &lastThread,
		&act.RepliesToday, &oldestReply,
		&act.ThreadsToday, &oldestThread,
		&act.FlagsToday, &oldestFlg,
		&act.VisibleReplies)
	if err != nil {
		return nil, fmt.Errorf("activity: %w", err)
	}
	if act.LastReplyAt, err = nullTime(lastReply); err != nil {
		return nil, err
	}
	if act.LastThreadAt, err = nullTime(lastThread); err != nil {
		return nil, err
	}
	if act.OldestReplyInWindow, err = nullTime(oldestReply); err != nil {
		return nil, err
	}
	if act.OldestThreadInWindow, err = nullTime(oldestThread); err != nil {
		return nil, err
	}
	if act.OldestFlagInWindow, err = nullTime(oldestFlg); err != nil {
		return nil, err
	}
	return &act, nil
}
