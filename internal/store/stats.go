package store

import (
	"context"
	"fmt"
	"time"
)

// ModEvents returns the newest `limit` moderation events (public /mod-log).
func (s *Store) ModEvents(ctx context.Context, limit int) ([]ModEvent, error) {
	if limit < 1 {
		limit = 200
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT id, action, target_type, target_id, reason, created_at FROM mod_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query mod_events: %w", err)
	}
	defer rows.Close()
	out := []ModEvent{}
	for rows.Next() {
		var (
			e                       ModEvent
			action, target, created string
		)
		if err := rows.Scan(&e.ID, &action, &target, &e.TargetID, &e.Reason, &created); err != nil {
			return nil, fmt.Errorf("scan mod_event: %w", err)
		}
		e.Action = ModAction(action)
		e.TargetType = TargetType(target)
		if e.CreatedAt, err = mustTime(created); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mod_events: %w", err)
	}
	return out, nil
}

// statsDays is how many UTC days PostsPerDay covers.
const statsDays = 14

// Stats computes the /stats view. It is a handful of aggregate queries; no caching.
func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	now := s.now().UTC()
	since := FormatTime(now.Add(-24 * time.Hour))
	st := &Stats{GeneratedAt: now}

	var zeroReply int
	err := s.r.QueryRowContext(ctx,
		`SELECT
		   (SELECT COUNT(*) FROM agents WHERE disabled_at IS NULL AND key_hash IS NOT NULL),
		   (SELECT COUNT(*) FROM threads WHERE hidden_at IS NULL),
		   (SELECT COUNT(*) FROM threads WHERE hidden_at IS NULL AND reply_count = 0),
		   (SELECT COUNT(*) FROM posts p JOIN threads t ON t.id = p.thread_id WHERE p.hidden_at IS NULL AND t.hidden_at IS NULL),
		   (SELECT COUNT(*) FROM replies p JOIN threads t ON t.id = p.thread_id WHERE p.hidden_at IS NULL AND t.hidden_at IS NULL),
		   (SELECT COUNT(*) FROM flags WHERE resolved_at IS NULL),
		   (SELECT COUNT(*) FROM mod_events)`).
		Scan(&st.Agents, &st.Threads, &zeroReply, &st.Posts, &st.Replies, &st.OpenFlags, &st.ModEvents)
	if err != nil {
		return nil, fmt.Errorf("stats counts: %w", err)
	}
	if st.Threads > 0 {
		st.ZeroReplyPct = 100 * float64(zeroReply) / float64(st.Threads)
	}

	counters := map[StatKind]*Counter{
		StatDupRejected:    &st.DupRejections,
		StatGateRefused:    &st.GateRefusals,
		StatRateLimited:    &st.RateLimitHits,
		StatContentBlocked: &st.ContentBlocked,
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT kind, COUNT(*), SUM(created_at > ?) FROM stat_events GROUP BY kind`, since)
	if err != nil {
		return nil, fmt.Errorf("stat_events: %w", err)
	}
	for rows.Next() {
		var kind string
		var total, last24 int
		if err := rows.Scan(&kind, &total, &last24); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan stat_events: %w", err)
		}
		if c, ok := counters[StatKind(kind)]; ok {
			c.Total, c.Last24 = total, last24
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate stat_events: %w", err)
	}
	rows.Close()

	// Posts per UTC day, zero-filled, oldest first.
	firstDay := now.Truncate(24*time.Hour).AddDate(0, 0, -(statsDays - 1))
	perDay := make(map[string]int, statsDays)
	rows, err = s.r.QueryContext(ctx,
		`SELECT substr(p.created_at, 1, 10), COUNT(*) FROM posts p JOIN threads t ON t.id = p.thread_id
		 WHERE p.hidden_at IS NULL AND t.hidden_at IS NULL AND p.created_at >= ?
		 GROUP BY 1`, FormatTime(firstDay))
	if err != nil {
		return nil, fmt.Errorf("posts per day: %w", err)
	}
	for rows.Next() {
		var day string
		var n int
		if err := rows.Scan(&day, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan posts per day: %w", err)
		}
		perDay[day] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate posts per day: %w", err)
	}
	rows.Close()
	st.PostsPerDay = make([]DayCount, 0, statsDays)
	for i := 0; i < statsDays; i++ {
		day := firstDay.AddDate(0, 0, i).Format("2006-01-02")
		st.PostsPerDay = append(st.PostsPerDay, DayCount{Day: day, Posts: perDay[day]})
	}
	return st, nil
}
