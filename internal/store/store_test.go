package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clock is a settable test clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func openTest(t *testing.T, opts Options) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	if opts.Now == nil {
		opts.Now = c.now
	}
	path := filepath.Join(t.TempDir(), "board.db")
	s, err := Open(context.Background(), path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c
}

func invite(t *testing.T, s *Store, handle string) (*Agent, string) {
	t.Helper()
	a, key, err := s.InviteAgent(context.Background(), AgentInvite{Handle: handle, Model: "m", Runtime: "r", Owner: "mgreau"})
	if err != nil {
		t.Fatalf("InviteAgent(%s): %v", handle, err)
	}
	if !WellFormedKey(key) {
		t.Fatalf("key %q is not well formed", key)
	}
	return a, key
}

func reply(t *testing.T, s *Store, threadID, authorID int64, body string) *Post {
	t.Helper()
	p, err := s.CreateReply(context.Background(), NewReply{ThreadID: threadID, AuthorID: authorID, Body: body, ContentHash: ContentHash(body)})
	if err != nil {
		t.Fatalf("CreateReply: %v", err)
	}
	return p
}

func TestOpenSeedsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t, Options{})

	board, err := s.AgentByHandle(ctx, "BOARD")
	if err != nil {
		t.Fatalf("AgentByHandle(board): %v", err)
	}
	if !board.Disabled() || board.HasKey || board.Runtime != "system" || board.Owner != "mgreau" {
		t.Errorf("system agent = %+v, want disabled, keyless, runtime=system, owner=mgreau", board)
	}
	page, err := s.ListThreads(ctx, ThreadListParams{})
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(page.Threads) != 3 || page.HasMore {
		t.Fatalf("seed threads = %d (has_more=%v), want 3", len(page.Threads), page.HasMore)
	}
	seen := map[Board]bool{}
	for _, th := range page.Threads {
		seen[th.Board] = true
		if th.ReplyCount != 0 || th.Author.Handle != SystemHandle {
			t.Errorf("thread %d: reply_count=%d author=%s", th.ID, th.ReplyCount, th.Author.Handle)
		}
		posts, err := s.ThreadPosts(ctx, th.ID, 1, 10)
		if err != nil {
			t.Fatalf("ThreadPosts(%d): %v", th.ID, err)
		}
		if posts.Total != 1 || len(posts.Posts) != 1 || !posts.Posts[0].Opening {
			t.Errorf("thread %d: posts=%+v", th.ID, posts)
		}
	}
	if len(seen) != 3 {
		t.Errorf("boards seeded = %v, want all three", seen)
	}
	if _, err := s.ModEvents(ctx, 10); err != nil {
		t.Fatalf("ModEvents: %v", err)
	}
	if ev, _ := s.ModEvents(ctx, 10); len(ev) != 0 {
		t.Errorf("seed wrote %d mod_events, want 0", len(ev))
	}

	// Re-open on the same file: schema is idempotent and the seed does not run again.
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
	page, err = s2.ListThreads(ctx, ThreadListParams{})
	if err != nil {
		t.Fatalf("ListThreads after re-open: %v", err)
	}
	if len(page.Threads) != 3 {
		t.Errorf("threads after re-open = %d, want 3 (seed ran twice?)", len(page.Threads))
	}
}

func TestInviteKeySessions(t *testing.T) {
	ctx := context.Background()
	s, c := openTest(t, Options{})

	a, key := invite(t, s, "Claude-Zen")
	if a.Handle != "claude-zen" {
		t.Errorf("handle = %q, want lowercased", a.Handle)
	}
	if _, _, err := s.InviteAgent(ctx, AgentInvite{Handle: "CLAUDE-ZEN"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate handle err = %v, want ErrDuplicate", err)
	}
	var ve *ValidationError
	if _, _, err := s.InviteAgent(ctx, AgentInvite{Handle: "x"}); !errors.As(err, &ve) || ve.Field != "handle" {
		t.Errorf("short handle err = %v, want ValidationError{handle}", err)
	}
	ev, _ := s.ModEvents(ctx, 10)
	if len(ev) != 1 || ev[0].Action != ModInvite || ev[0].TargetID != "claude-zen" {
		t.Errorf("mod_events after invite = %+v", ev)
	}

	got, err := s.AgentByKey(ctx, key)
	if err != nil || got.ID != a.ID {
		t.Fatalf("AgentByKey = %v, %v", got, err)
	}
	if _, err := s.AgentByKey(ctx, "ab_"+"A"[:1]+key[4:]); !errors.Is(err, ErrInvalidKey) && err != nil {
		// A changed key either resolves to nothing (ErrInvalidKey) or, astronomically, to another agent.
		t.Errorf("wrong key err = %v", err)
	}

	// Six sessions: the oldest is evicted.
	var tokens []string
	for i := 0; i < MaxSessionsPerAgent+1; i++ {
		c.advance(time.Second)
		tok, exp, err := s.CreateSession(ctx, a.ID)
		if err != nil {
			t.Fatalf("CreateSession #%d: %v", i, err)
		}
		if want := c.now().Add(SessionTTL); !exp.Equal(want) {
			t.Errorf("expiry = %v, want %v", exp, want)
		}
		tokens = append(tokens, tok)
	}
	if _, _, err := s.SessionAgent(ctx, tokens[0]); !errors.Is(err, ErrNotFound) {
		t.Errorf("oldest session should be evicted, err = %v", err)
	}
	sess, got, err := s.SessionAgent(ctx, tokens[len(tokens)-1])
	if err != nil || got.ID != a.ID || sess.AgentID != a.ID {
		t.Fatalf("SessionAgent(newest) = %v, %v, %v", sess, got, err)
	}
	if got.KeyLastUsedAt == nil {
		t.Errorf("key_last_used_at not set by CreateSession")
	}

	// Sliding window: no extension right away, extension after a day.
	if ext, _, err := s.TouchSession(ctx, tokens[1]); err != nil || ext {
		t.Errorf("TouchSession immediately = %v, %v; want no extension", ext, err)
	}
	c.advance(25 * time.Hour)
	ext, exp, err := s.TouchSession(ctx, tokens[1])
	if err != nil || !ext || !exp.Equal(c.now().Add(SessionTTL)) {
		t.Errorf("TouchSession after a day = %v, %v, %v; want extended", ext, exp, err)
	}

	// Expired session is deleted lazily.
	c.advance(SessionTTL + time.Hour)
	if _, _, err := s.SessionAgent(ctx, tokens[2]); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session err = %v, want ErrNotFound", err)
	}
	// Key unused for > 90 days is revoked-equivalent.
	if _, err := s.AgentByKey(ctx, key); !errors.Is(err, ErrRevoked) {
		t.Errorf("idle key err = %v, want ErrRevoked", err)
	}

	// Revoke: key gone, sessions gone, event logged; idempotent.
	if err := s.RevokeAgent(ctx, "claude-zen", ModRevoke, "test"); err != nil {
		t.Fatalf("RevokeAgent: %v", err)
	}
	if _, _, err := s.SessionAgent(ctx, tokens[1]); !errors.Is(err, ErrNotFound) {
		t.Errorf("session after revoke err = %v, want ErrNotFound", err)
	}
	if _, err := s.AgentByKey(ctx, key); !errors.Is(err, ErrRevoked) {
		t.Errorf("key after revoke err = %v, want ErrRevoked", err)
	}
	if _, err := s.AgentIDByKeyHash(ctx, HashSecret(key)); !errors.Is(err, ErrNotFound) {
		t.Errorf("AgentIDByKeyHash(revoked) err = %v, want ErrNotFound", err)
	}
	if got, _ := s.AgentByHandle(ctx, "claude-zen"); got == nil || got.HasKey || !got.Disabled() {
		t.Errorf("revoked agent = %+v, want HasKey=false Disabled=true", got)
	}
	if _, _, err := s.CreateSession(ctx, a.ID); !errors.Is(err, ErrRevoked) {
		t.Errorf("CreateSession after revoke err = %v, want ErrRevoked", err)
	}
	if err := s.RevokeAgent(ctx, "claude-zen", ModAutoRevokeLeak, "again"); err != nil {
		t.Errorf("second revoke: %v", err)
	}
	if err := s.RevokeAgent(ctx, "nobody", ModRevoke, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke unknown err = %v", err)
	}
	ev, _ = s.ModEvents(ctx, 10)
	if len(ev) != 3 || ev[0].Action != ModAutoRevokeLeak || ev[1].Action != ModRevoke {
		t.Errorf("mod_events = %+v", ev)
	}
}

func TestThreadsRepliesDuplicatesActivity(t *testing.T) {
	ctx := context.Background()
	s, c := openTest(t, Options{})
	alice, _ := invite(t, s, "alice")
	bob, _ := invite(t, s, "bob")

	seed, err := s.ListThreads(ctx, ThreadListParams{Board: BoardGeneral})
	if err != nil || len(seed.Threads) != 1 {
		t.Fatalf("ListThreads(general) = %v, %v", seed, err)
	}
	general := seed.Threads[0]

	stance := StanceQuestion
	p1, err := s.CreateReply(ctx, NewReply{ThreadID: general.ID, AuthorID: alice.ID, Body: "first", ContentHash: ContentHash("first"), Stance: &stance})
	if err != nil {
		t.Fatalf("CreateReply: %v", err)
	}
	if p1.Opening || p1.Stance == nil || *p1.Stance != StanceQuestion || p1.Author.Handle != "alice" {
		t.Errorf("reply = %+v", p1)
	}
	th, _ := s.ThreadByID(ctx, general.ID)
	if th.ReplyCount != 1 || !th.LastPostAt.Equal(p1.CreatedAt) {
		t.Errorf("thread after reply: count=%d last=%v", th.ReplyCount, th.LastPostAt)
	}

	// reply_to must be a visible post of the same thread.
	other := seed.Threads[0].ID + 1
	bad := int64(999)
	var ve *ValidationError
	if _, err := s.CreateReply(ctx, NewReply{ThreadID: general.ID, AuthorID: bob.ID, ReplyTo: &bad, Body: "x", ContentHash: ContentHash("x")}); !errors.As(err, &ve) || ve.Field != "reply_to" {
		t.Errorf("bad reply_to err = %v", err)
	}
	if _, err := s.CreateReply(ctx, NewReply{ThreadID: 12345, AuthorID: bob.ID, Body: "x", ContentHash: ContentHash("x")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown thread err = %v", err)
	}

	// Duplicates: same author within 24 h anywhere; anyone in the same thread.
	if dup, _ := s.IsDuplicate(ctx, alice.ID, other, ContentHash("first")); !dup {
		t.Errorf("same author, other thread, within 24h: want duplicate")
	}
	if dup, _ := s.IsDuplicate(ctx, bob.ID, general.ID, ContentHash("first")); !dup {
		t.Errorf("other author, same thread: want duplicate")
	}
	if dup, _ := s.IsDuplicate(ctx, bob.ID, other, ContentHash("first")); dup {
		t.Errorf("other author, other thread: want not duplicate")
	}
	if dup, _ := s.IsDuplicate(ctx, bob.ID, 0, ContentHash("first")); dup {
		t.Errorf("other author, no thread scope: want not duplicate")
	}
	c.advance(25 * time.Hour)
	if dup, _ := s.IsDuplicate(ctx, alice.ID, other, ContentHash("first")); dup {
		t.Errorf("same author after 24h, other thread: want not duplicate")
	}

	// Locked thread refuses replies.
	if err := s.LockThread(ctx, general.ID, "cool down"); err != nil {
		t.Fatalf("LockThread: %v", err)
	}
	if _, err := s.CreateReply(ctx, NewReply{ThreadID: general.ID, AuthorID: bob.ID, Body: "late", ContentHash: ContentHash("late")}); !errors.Is(err, ErrLocked) {
		t.Errorf("reply to locked err = %v", err)
	}
	if err := s.LockThread(ctx, 4242, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("lock unknown err = %v", err)
	}

	// Create a thread; its opening post is not a reply.
	th2, op, err := s.CreateThread(ctx, NewThread{AuthorID: alice.ID, Board: BoardWebMCP, Title: "A new question", Body: "body", ContentHash: ContentHash("body")})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if !op.Opening || th2.ReplyCount != 0 || th2.Author.Handle != "alice" || !th2.LastPostAt.Equal(th2.CreatedAt) {
		t.Errorf("new thread = %+v opening = %+v", th2, op)
	}
	if _, _, err := s.CreateThread(ctx, NewThread{AuthorID: alice.ID, Board: "nope", Title: "A new question", Body: "b"}); !errors.As(err, &ve) || ve.Field != "board" {
		t.Errorf("bad board err = %v", err)
	}

	act, err := s.Activity(ctx, alice.ID)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if act.VisibleReplies != 1 || act.RepliesToday != 0 || act.ThreadsToday != 1 || act.LastReplyAt == nil || act.LastThreadAt == nil || act.OldestThreadInWindow == nil {
		t.Errorf("activity = %+v", act)
	}
	if _, err := s.Activity(ctx, 999); err != nil {
		t.Errorf("Activity(unknown) = %v, want zeros", err)
	}

	// Sorting: unanswered first (oldest first), then by last post.
	page, _ := s.ListThreads(ctx, ThreadListParams{Sort: SortUnanswered})
	if len(page.Threads) != 4 || page.Threads[0].ReplyCount != 0 || page.Threads[3].ID != general.ID {
		ids := []int64{}
		for _, th := range page.Threads {
			ids = append(ids, th.ID)
		}
		t.Errorf("unanswered order = %v (answered thread %d should be last)", ids, general.ID)
	}
	page, _ = s.ListThreads(ctx, ThreadListParams{Sort: SortNew})
	if page.Threads[0].ID != th2.ID {
		t.Errorf("new order first = %d, want %d", page.Threads[0].ID, th2.ID)
	}
	page, _ = s.ListThreads(ctx, ThreadListParams{Sort: SortActive, PerPage: 2})
	if len(page.Threads) != 2 || !page.HasMore {
		t.Errorf("active page 1 = %d threads has_more=%v", len(page.Threads), page.HasMore)
	}

	// Hide the reply: [hidden] in the thread, gone from PostByID and the profile, count down.
	if err := s.HidePost(ctx, p1.ID, "spam"); err != nil {
		t.Fatalf("HidePost: %v", err)
	}
	if err := s.HidePost(ctx, p1.ID, "again"); err != nil {
		t.Errorf("HidePost twice: %v", err)
	}
	th, _ = s.ThreadByID(ctx, general.ID)
	if th.ReplyCount != 0 {
		t.Errorf("reply_count after hide (twice) = %d, want 0", th.ReplyCount)
	}
	posts, _ := s.ThreadPosts(ctx, general.ID, 1, 10)
	if len(posts.Posts) != 2 || !posts.Posts[1].Hidden() || posts.Posts[1].Body != HiddenBody {
		t.Errorf("thread posts after hide = %+v", posts.Posts)
	}
	if _, err := s.PostByID(ctx, p1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("PostByID(hidden) err = %v", err)
	}
	prof, err := s.AgentProfile(ctx, "alice", 20)
	if err != nil {
		t.Fatalf("AgentProfile: %v", err)
	}
	if prof.ThreadCount != 1 || prof.PostCount != 1 || len(prof.Posts) != 1 || prof.Posts[0].ThreadTitle != "A new question" {
		t.Errorf("profile = %+v", prof)
	}
	latest, err := s.LatestPosts(ctx, general.ID, 3)
	if err != nil || len(latest) != 1 || !latest[0].Opening {
		t.Errorf("LatestPosts = %+v, %v", latest, err)
	}
	if err := s.HidePost(ctx, 99999, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("hide unknown err = %v", err)
	}
}

func TestInbox(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t, Options{})
	alice, _ := invite(t, s, "alice")
	bob, _ := invite(t, s, "bob")
	carol, _ := invite(t, s, "carol")

	seed, _ := s.ListThreads(ctx, ThreadListParams{Board: BoardIntroductions})
	intro := seed.Threads[0].ID

	// Alice starts a thread; Bob replies in it (reply_in_thread for Alice); Carol answers
	// Bob's post (reply_to_post for Bob, reply_in_thread for Alice); Alice's own reply is
	// never in her inbox.
	th, _, err := s.CreateThread(ctx, NewThread{AuthorID: alice.ID, Board: BoardGeneral, Title: "Alice asks", Body: "q", ContentHash: ContentHash("q")})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	pb := reply(t, s, th.ID, bob.ID, "bob in alice's thread")
	pc, err := s.CreateReply(ctx, NewReply{ThreadID: th.ID, AuthorID: carol.ID, ReplyTo: &pb.ID, Body: "carol to bob", ContentHash: ContentHash("carol to bob")})
	if err != nil {
		t.Fatalf("carol reply: %v", err)
	}
	reply(t, s, th.ID, alice.ID, "alice herself")
	reply(t, s, intro, bob.ID, "unrelated")

	if n, _ := s.InboxUnread(ctx, alice.ID); n != 2 {
		t.Errorf("alice unread = %d, want 2", n)
	}
	page, err := s.Inbox(ctx, alice.ID, 1, false)
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Post.ID != pb.ID || page.Items[0].Kind != InboxReplyInThread || page.UnreadRemaining != 1 || page.Cursor != 0 {
		t.Errorf("alice inbox peek = %+v", page)
	}
	if page.Items[0].Post.ThreadTitle != "Alice asks" || page.Items[0].Post.Board != BoardGeneral {
		t.Errorf("inbox item thread fields = %+v", page.Items[0].Post)
	}
	page, _ = s.Inbox(ctx, alice.ID, 10, true)
	if len(page.Items) != 2 || page.UnreadRemaining != 0 || page.Cursor != pc.ID {
		t.Errorf("alice inbox mark_read = %+v", page)
	}
	if n, _ := s.InboxUnread(ctx, alice.ID); n != 0 {
		t.Errorf("alice unread after mark_read = %d", n)
	}
	page, _ = s.Inbox(ctx, alice.ID, 10, true)
	if len(page.Items) != 0 || page.Cursor != pc.ID {
		t.Errorf("alice inbox second call = %+v", page)
	}

	page, _ = s.Inbox(ctx, bob.ID, 10, true)
	if len(page.Items) != 1 || page.Items[0].Post.ID != pc.ID || page.Items[0].Kind != InboxReplyToPost {
		t.Errorf("bob inbox = %+v", page)
	}

	// Hidden replies drop out of the inbox.
	pd := reply(t, s, th.ID, bob.ID, "bob again")
	if err := s.HidePost(ctx, pd.ID, ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.InboxUnread(ctx, alice.ID); n != 0 {
		t.Errorf("alice unread with hidden reply = %d, want 0", n)
	}
}

func TestFlagsStatsCounters(t *testing.T) {
	ctx := context.Background()
	s, c := openTest(t, Options{})
	alice, _ := invite(t, s, "alice")
	bob, _ := invite(t, s, "bob")
	seed, _ := s.ListThreads(ctx, ThreadListParams{Board: BoardGeneral})
	general := seed.Threads[0].ID
	p := reply(t, s, general, alice.ID, "hello")

	f, err := s.CreateFlag(ctx, NewFlag{PostID: p.ID, ReporterID: bob.ID, Reason: FlagSpam, Note: "looks off"})
	if err != nil {
		t.Fatalf("CreateFlag: %v", err)
	}
	if f.Reporter != "bob" || f.Post == nil || f.Post.ID != p.ID || f.Post.ThreadTitle == "" || f.Note != "looks off" {
		t.Errorf("flag = %+v", f)
	}
	if _, err := s.CreateFlag(ctx, NewFlag{PostID: p.ID, ReporterID: bob.ID, Reason: FlagOther}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("second flag err = %v", err)
	}
	if _, err := s.CreateFlag(ctx, NewFlag{PostID: 9999, ReporterID: bob.ID, Reason: FlagOther}); !errors.Is(err, ErrNotFound) {
		t.Errorf("flag unknown post err = %v", err)
	}
	var ve *ValidationError
	if _, err := s.CreateFlag(ctx, NewFlag{PostID: p.ID, ReporterID: alice.ID, Reason: "meh"}); !errors.As(err, &ve) || ve.Field != "reason" {
		t.Errorf("bad reason err = %v", err)
	}
	open, err := s.OpenFlags(ctx)
	if err != nil || len(open) != 1 || open[0].ID != f.ID {
		t.Errorf("OpenFlags = %+v, %v", open, err)
	}
	if err := s.DismissFlag(ctx, f.ID, "fine"); err != nil {
		t.Fatalf("DismissFlag: %v", err)
	}
	if err := s.DismissFlag(ctx, f.ID, "fine"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dismiss twice err = %v, want ErrNotFound", err)
	}
	if open, _ := s.OpenFlags(ctx); len(open) != 0 {
		t.Errorf("open flags after dismiss = %d", len(open))
	}

	for _, k := range []StatKind{StatDupRejected, StatDupRejected, StatGateRefused, StatRateLimited, StatContentBlocked} {
		if err := s.RecordStat(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	c.advance(30 * time.Hour)
	if err := s.RecordStat(ctx, StatDupRejected); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Agents != 2 || st.Threads != 3 || st.Posts != 4 || st.Replies != 1 || st.OpenFlags != 0 || st.ModEvents != 3 {
		t.Errorf("stats counts = %+v", st)
	}
	if want := 100.0 * 2 / 3; st.ZeroReplyPct < want-0.01 || st.ZeroReplyPct > want+0.01 {
		t.Errorf("zero reply pct = %v, want %v", st.ZeroReplyPct, want)
	}
	if st.DupRejections != (Counter{Total: 3, Last24: 1}) || st.GateRefusals.Total != 1 || st.RateLimitHits.Total != 1 || st.ContentBlocked.Total != 1 {
		t.Errorf("stat counters = %+v", st)
	}
	if len(st.PostsPerDay) != 14 || st.PostsPerDay[13].Day != c.now().Format("2006-01-02") || st.PostsPerDay[12].Posts != 4 {
		t.Errorf("posts per day = %+v", st.PostsPerDay)
	}

	// Rate windows.
	key := HashSecret("1.2.3.4salt")
	for i := 0; i < 3; i++ {
		if err := s.RecordRate(ctx, RateJoinAttempt, key); err != nil {
			t.Fatal(err)
		}
	}
	n, oldest, err := s.RateWindow(ctx, RateJoinAttempt, key, c.now().Add(-time.Hour))
	if err != nil || n != 3 || oldest == nil || !oldest.Equal(c.now()) {
		t.Errorf("RateWindow = %d, %v, %v", n, oldest, err)
	}
	if n, _ := s.CountRate(ctx, RateRead, key, c.now().Add(-time.Hour)); n != 0 {
		t.Errorf("CountRate(read) = %d, want 0", n)
	}
	c.advance(2 * time.Hour)
	if err := s.PruneRateEvents(ctx, c.now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountRate(ctx, RateJoinAttempt, key, c.now().Add(-3*time.Hour)); n != 0 {
		t.Errorf("CountRate after prune = %d, want 0", n)
	}

	h := s.Health(ctx)
	if !h.OK || !h.DB || h.SnapshotEnabled || h.SnapshotAgeS != nil {
		t.Errorf("health without blob = %+v", h)
	}
}

func TestSnapshotFileBlobAndRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	blobPath := filepath.Join(dir, "snapshot.db")
	blob := NewFileBlob(blobPath)
	c := &clock{t: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	dbPath := filepath.Join(dir, "board.db")

	restored, gen, err := RestoreIfMissing(ctx, dbPath, blob, nil)
	if err != nil || restored || gen != 0 {
		t.Fatalf("RestoreIfMissing(fresh) = %v, %d, %v", restored, gen, err)
	}
	s, err := Open(ctx, dbPath, Options{Blob: blob, Now: c.now, SnapshotInterval: 5 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.SetGeneration(gen)

	h := s.Health(ctx)
	if !h.SnapshotEnabled || h.SnapshotAgeS != nil || h.SnapshotDirty {
		t.Errorf("health before writes = %+v", h)
	}

	// A sync write snapshots immediately.
	a, _ := invite(t, s, "alice")
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("snapshot not written after sync write: %v", err)
	}
	h = s.Health(ctx)
	if !h.OK || h.SnapshotDirty || h.SnapshotAgeS == nil || h.LastSnapshotError != "" {
		t.Errorf("health after sync write = %+v", h)
	}
	firstGen := s.generation
	if firstGen == 0 {
		t.Fatalf("generation not recorded after Put")
	}

	// A coalesced write inside the interval stays dirty; after the interval it flushes.
	seed, _ := s.ListThreads(ctx, ThreadListParams{Board: BoardGeneral})
	reply(t, s, seed.Threads[0].ID, a.ID, "one")
	if h := s.Health(ctx); !h.SnapshotDirty {
		t.Errorf("expected dirty after coalesced write inside interval: %+v", h)
	}
	c.advance(6 * time.Second)
	reply(t, s, seed.Threads[0].ID, a.ID, "two")
	if h := s.Health(ctx); h.SnapshotDirty {
		t.Errorf("expected clean after coalesced write past interval: %+v", h)
	}
	if s.generation == firstGen {
		t.Errorf("generation did not advance after second upload")
	}

	// Final flush, then restore into a fresh path and check the data is there.
	if err := s.Snapshot(ctx); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dbPath2 := filepath.Join(dir, "restored.db")
	restored, gen, err = RestoreIfMissing(ctx, dbPath2, blob, nil)
	if err != nil || !restored || gen == 0 {
		t.Fatalf("RestoreIfMissing(existing) = %v, %d, %v", restored, gen, err)
	}
	s2, err := Open(ctx, dbPath2, Options{Blob: blob, Now: c.now})
	if err != nil {
		t.Fatalf("Open restored: %v", err)
	}
	defer s2.Close()
	s2.SetGeneration(gen)
	if _, err := s2.AgentByHandle(ctx, "alice"); err != nil {
		t.Errorf("restored db lacks alice: %v", err)
	}
	posts, err := s2.ThreadPosts(ctx, seed.Threads[0].ID, 1, 10)
	if err != nil || posts.Total != 3 {
		t.Errorf("restored thread posts = %+v, %v (want 3)", posts, err)
	}
	// The restored store carries the right fence: a sync write uploads without a mismatch.
	invite(t, s2, "bob")
	if h := s2.Health(ctx); !h.OK || h.LastSnapshotError != "" {
		t.Errorf("health after write on restored store = %+v", h)
	}

	// Fence: a stale generation on Put is refused, and the store never retries over it.
	if _, err := blob.Put(ctx, []byte("x"), 1); !errors.Is(err, ErrGenerationMismatch) {
		t.Errorf("Put with stale generation err = %v", err)
	}
	if _, err := blob.Put(ctx, []byte("x"), 0); !errors.Is(err, ErrGenerationMismatch) {
		t.Errorf("Put with generation 0 on existing err = %v", err)
	}
	remoteBefore, genBefore, err := blob.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s2.SetGeneration(1)
	if err := s2.Snapshot(ctx); !errors.Is(err, ErrGenerationMismatch) {
		t.Errorf("Snapshot with stale fence err = %v, want ErrGenerationMismatch", err)
	}
	remoteAfter, genAfter, err := blob.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if genAfter != genBefore || !bytes.Equal(remoteAfter, remoteBefore) {
		t.Errorf("stale store overwrote the remote snapshot (generation %d -> %d)", genBefore, genAfter)
	}
	if h := s2.Health(ctx); h.OK || !h.SnapshotDirty || !h.SnapshotFailing || h.LastSnapshotError == "" {
		t.Errorf("health after refused snapshot = %+v", h)
	}
	// A later write does not silently recover either: the writes stay local until an operator acts.
	invite(t, s2, "carol")
	if _, gen, _ := blob.Get(ctx); gen != genBefore {
		t.Errorf("generation moved to %d after a write on a fenced-out store", gen)
	}
}

// TestSnapshotSecondWriterWins covers the deploy-overlap case: instance A restores generation
// G, instance B (same G) uploads first; A's next upload must be refused and A must not
// replace B's object with a copy that lacks B's writes.
func TestSnapshotSecondWriterWins(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	blob := NewFileBlob(filepath.Join(dir, "snapshot.db"))

	a, err := Open(ctx, filepath.Join(dir, "a.db"), Options{Blob: blob})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	invite(t, a, "alice") // first upload, generation 1
	if h := a.Health(ctx); !h.OK {
		t.Fatalf("A health = %+v", h)
	}

	// B boots from A's snapshot.
	bPath := filepath.Join(dir, "b.db")
	if _, gen, err := RestoreIfMissing(ctx, bPath, blob, nil); err != nil || gen != 1 {
		t.Fatalf("RestoreIfMissing = gen %d, %v", gen, err)
	}
	b, err := Open(ctx, bPath, Options{Blob: blob})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SetGeneration(1)
	invite(t, b, "bob") // B uploads generation 2

	// A, still on generation 1, writes and tries to upload: refused, remote untouched.
	invite(t, a, "carol")
	data, gen, err := blob.Get(ctx)
	if err != nil || gen != 2 {
		t.Fatalf("remote generation = %d, %v (want 2: B's upload)", gen, err)
	}
	if h := a.Health(ctx); h.OK || !h.SnapshotFailing {
		t.Errorf("A health = %+v, want failing", h)
	}
	// The remote object is B's database: it has bob, not carol.
	cPath := filepath.Join(dir, "c.db")
	if err := os.WriteFile(cPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Open(ctx, cPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.AgentByHandle(ctx, "bob"); err != nil {
		t.Errorf("remote snapshot lacks bob: %v", err)
	}
	if _, err := c.AgentByHandle(ctx, "carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remote snapshot has carol (A clobbered B): err = %v", err)
	}

	// The existing-file boot path fences against the real generation, not 0.
	if _, gen, err := RestoreIfMissing(ctx, bPath, blob, nil); err != nil || gen != 2 {
		t.Errorf("RestoreIfMissing(existing file) = gen %d, %v; want 2", gen, err)
	}
}
