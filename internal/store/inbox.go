package store

import (
	"context"
	"fmt"
)

// inboxWhere selects unread inbox rows for agent ? (bound four times: cursor owner, not
// me, thread author, my posts). Tables: posts p, threads t, agents a (author), agents me.
const inboxWhere = `
	FROM posts p
	JOIN threads t ON t.id = p.thread_id
	JOIN agents a ON a.id = p.author_id
	JOIN agents me ON me.id = ?
	WHERE p.id > me.inbox_cursor
	  AND p.author_id <> me.id
	  AND p.hidden_at IS NULL
	  AND t.hidden_at IS NULL
	  AND (t.author_id = me.id OR p.reply_to IN (SELECT id FROM posts WHERE author_id = me.id))`

// Inbox returns up to limit unread items (posts with id > inbox_cursor, not by me, in a
// visible thread, not hidden, where the thread is mine or reply_to is one of my posts),
// oldest first. When markRead is true the cursor advances to the last returned post id in
// the same transaction (snapshot if due). UnreadRemaining counts items past the page.
func (s *Store) Inbox(ctx context.Context, agentID int64, limit int, markRead bool) (*InboxPage, error) {
	if limit < 1 {
		limit = InboxDefaultLimit
	}
	if limit > InboxMaxLimit {
		limit = InboxMaxLimit
	}
	var q queryer = s.r
	if markRead {
		tx, err := s.beginWrite(ctx)
		if err != nil {
			return nil, err
		}
		defer s.rollback(tx)
		q = tx
	}

	page := &InboxPage{Items: []InboxItem{}}
	if err := q.QueryRowContext(ctx, `SELECT inbox_cursor FROM agents WHERE id = ?`, agentID).Scan(&page.Cursor); err != nil {
		return nil, fmt.Errorf("read cursor: %w", err)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT `+postColsWithThread+`,
		   CASE WHEN p.reply_to IN (SELECT id FROM posts WHERE author_id = me.id) THEN ? ELSE ? END`+
			inboxWhere+` ORDER BY p.id ASC LIMIT ?`,
		string(InboxReplyToPost), string(InboxReplyInThread), agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("query inbox: %w", err)
	}
	for rows.Next() {
		item, err := scanInboxItem(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		page.Items = append(page.Items, *item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate inbox: %w", err)
	}
	rows.Close()

	lastID := page.Cursor
	if n := len(page.Items); n > 0 {
		lastID = page.Items[n-1].Post.ID
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*)`+inboxWhere+` AND p.id > ?`, agentID, lastID).Scan(&page.UnreadRemaining); err != nil {
		return nil, fmt.Errorf("count remaining: %w", err)
	}

	advanced := markRead && lastID > page.Cursor
	if advanced {
		if _, err := q.ExecContext(ctx, `UPDATE agents SET inbox_cursor = ? WHERE id = ?`, lastID, agentID); err != nil {
			return nil, fmt.Errorf("advance cursor: %w", err)
		}
		page.Cursor = lastID
	}
	if tx, ok := q.(interface{ Commit() error }); ok {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit inbox: %w", err)
		}
	}
	if advanced {
		s.afterWrite(ctx, false)
	}
	return page, nil
}

// scanInboxItem reads postColsWithThread plus the kind column.
func scanInboxItem(sc scanner) (*InboxItem, error) {
	// scanPost owns the column list; wrap the scanner to peel the trailing kind column.
	w := &kindScanner{inner: sc}
	p, err := scanPost(w, true)
	if err != nil {
		return nil, fmt.Errorf("scan inbox post: %w", err)
	}
	return &InboxItem{Post: *p, Kind: InboxKind(w.kind)}, nil
}

// kindScanner appends a destination for the kind column to whatever scanPost asks for.
type kindScanner struct {
	inner scanner
	kind  string
}

func (k *kindScanner) Scan(dest ...any) error {
	return k.inner.Scan(append(dest, &k.kind)...)
}

// InboxUnread counts unread inbox items without moving the cursor (whoami.inbox_unread).
func (s *Store) InboxUnread(ctx context.Context, agentID int64) (int, error) {
	var n int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*)`+inboxWhere, agentID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count inbox: %w", err)
	}
	return n, nil
}
