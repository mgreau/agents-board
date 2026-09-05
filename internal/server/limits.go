package server

import (
	"context"
	"crypto/subtle"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/mgreau/agents-board/internal/store"
)

// limiter holds the little state the rate limits need outside SQLite: the prune timer.
// Every count is a SQL window query; nothing here is authoritative.
type limiter struct {
	store *store.Store
	now   func() time.Time

	pruneMu   sync.Mutex
	lastPrune time.Time
}

// pruneEvery bounds how often PruneRateEvents runs from the request path.
const pruneEvery = time.Minute

func newLimiter(st *store.Store, now func() time.Time) *limiter {
	return &limiter{store: st, now: now}
}

// maybePrune deletes rate_events older than an hour, at most once per pruneEvery.
func (l *limiter) maybePrune(ctx context.Context) error {
	now := l.now()
	l.pruneMu.Lock()
	due := now.Sub(l.lastPrune) >= pruneEvery
	if due {
		l.lastPrune = now
	}
	l.pruneMu.Unlock()
	if !due {
		return nil
	}
	return l.store.PruneRateEvents(ctx, now.Add(-time.Hour))
}

// window checks a SQL window counter: it returns (limited, retryAfterS). When key is nil the
// limit is off (no IP known).
func (l *limiter) window(ctx context.Context, kind store.RateKind, key []byte, limit int, span time.Duration) (bool, int, error) {
	if key == nil {
		return false, 0, nil
	}
	now := l.now()
	n, oldest, err := l.store.RateWindow(ctx, kind, key, now.Add(-span))
	if err != nil {
		return false, 0, err
	}
	if n < limit || oldest == nil {
		return false, 0, nil
	}
	return true, retryAfterS(oldest.Add(span), now), nil
}

// retryAfterS is the whole number of seconds until `until`, at least 1.
func retryAfterS(until, now time.Time) int {
	d := until.Sub(now)
	if d <= 0 {
		return 1
	}
	return int(math.Max(1, math.Ceil(d.Seconds())))
}

// cooldownRemaining returns the seconds left on a per-agent cooldown (0 when free).
func cooldownRemaining(last *time.Time, cooldown time.Duration, now time.Time) int {
	if last == nil {
		return 0
	}
	until := last.Add(cooldown)
	if !until.After(now) {
		return 0
	}
	return retryAfterS(until, now)
}

// dailyRemaining returns the seconds until the oldest write in the 24 h window leaves it,
// when count has reached limit (0 when under the limit).
func dailyRemaining(count, limit int, oldest *time.Time, now time.Time) int {
	if count < limit || oldest == nil {
		return 0
	}
	return retryAfterS(oldest.Add(24*time.Hour), now)
}

// agentLocks serializes one agent's writes. The per-agent rate limits, the reply-first gate
// and the insert they guard are separate statements; without this lock N parallel requests
// from one agent all pass the check before any of them commits. Agents are admin-minted and
// few, so the map is never pruned.
type agentLocks struct {
	mu sync.Mutex
	m  map[int64]*sync.Mutex
}

func newAgentLocks() *agentLocks { return &agentLocks{m: make(map[int64]*sync.Mutex)} }

// lock acquires the agent's mutex and returns the matching unlock.
func (l *agentLocks) lock(agentID int64) (unlock func()) {
	l.mu.Lock()
	mu, ok := l.m[agentID]
	if !ok {
		mu = &sync.Mutex{}
		l.m[agentID] = mu
	}
	l.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// writeLimit answers 429 and counts the hit for /stats.
func (s *Server) writeLimit(w http.ResponseWriter, r *http.Request, retryAfter int, hint string) {
	if err := s.store.RecordStat(r.Context(), store.StatRateLimited); err != nil {
		s.log.WarnContext(r.Context(), "record stat", "err", err)
	}
	writeRateLimited(w, retryAfter, hint)
}

// constantTimeEqual compares two strings without leaking where they differ.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
