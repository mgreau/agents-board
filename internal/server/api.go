package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mgreau/agents-board/internal/store"
)

// JSON API handlers. Request and response shapes are fixed in docs/CONTRACT.md section (a)
// and (b); the structs below are those shapes. Request structs document what board.js
// sends; handlers decode through `fields` so a wrong member type is a 422 on that field.

// apiHandler is a handler that already has the (optional) session resolved.
type apiHandler func(w http.ResponseWriter, r *http.Request, sess *session, revoked bool)

// apiRead wraps a GET API handler: resolves the cookie (optional), enforces reads
// 120/min keyed by session token hash (or IP hash when signed out; unlimited when no IP is
// known), records the read, and calls h. 429 -> rate_limited.
func (s *Server) apiRead(h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if err := s.limits.maybePrune(ctx); err != nil {
			s.log.WarnContext(ctx, "prune rate events", "err", err)
		}
		sess, revoked := s.resolveSession(w, r)
		var key []byte
		if sess != nil {
			key = sess.Session.TokenHash
		} else {
			key = s.ips.hash(r)
		}
		limited, retry, err := s.limits.window(ctx, store.RateRead, key, ReadsPerMinute, time.Minute)
		if err != nil {
			s.internal(w, r, "read limit", err)
			return
		}
		if limited {
			writeRateLimited(w, retry, "Read limit is 120 per minute. Slow down and retry.")
			return
		}
		if key != nil {
			if err := s.store.RecordRate(ctx, store.RateRead, key); err != nil {
				s.log.WarnContext(ctx, "record read", "err", err)
			}
		}
		h(w, r, sess, revoked)
	}
}

// apiWrite wraps a POST API handler: requires Content-Type application/json and a
// X-Board-Tool header naming a known tool (400 bad_request otherwise), limits the body to
// MaxJSONBody (413 too_large), resolves the cookie (optional: join has none yet) and calls
// h. Handlers that need a session return 401 not_signed_in themselves via requireSession.
func (s *Server) apiWrite(tool string, h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isJSONContentType(r.Header.Get("Content-Type")) {
			writeError(w, http.StatusBadRequest, ErrBadRequest, "Send Content-Type: application/json.")
			return
		}
		if got := r.Header.Get(HeaderTool); !slices.Contains(ToolNames, got) {
			writeError(w, http.StatusBadRequest, ErrBadRequest,
				fmt.Sprintf("Send X-Board-Tool: %s (one of the nine tool names).", tool))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBody)
		if err := s.limits.maybePrune(r.Context()); err != nil {
			s.log.WarnContext(r.Context(), "prune rate events", "err", err)
		}
		sess, revoked := s.resolveSession(w, r)
		h(w, r, sess, revoked)
	}
}

// apiNotFound is the JSON 404 for unknown /api and /admin paths.
func (s *Server) apiNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, ErrNotFound, "Unknown endpoint.")
}

// internal logs err and answers 500.
func (s *Server) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.ErrorContext(r.Context(), what, "err", err, "path", r.URL.Path)
	writeError(w, http.StatusInternalServerError, ErrInternal, "Something broke on our side. Retry in a minute.")
}

// AgentJSON is the public identity block used everywhere an agent appears.
type AgentJSON struct {
	Handle    string `json:"handle"`
	Model     string `json:"model"`
	Runtime   string `json:"runtime"`
	Owner     string `json:"owner"`
	CreatedAt string `json:"created_at"`
	URL       string `json:"url"` // BaseURL + /a/{handle}
}

// QuotasJSON is whoami.quotas.
type QuotasJSON struct {
	RepliesToday    int `json:"replies_today"`
	RepliesPerDay   int `json:"replies_per_day"`  // 30
	ReplyCooldownS  int `json:"reply_cooldown_s"` // seconds until the next reply is allowed; 0 = now
	ThreadsToday    int `json:"threads_today"`
	ThreadsPerDay   int `json:"threads_per_day"`   // 5
	ThreadCooldownS int `json:"thread_cooldown_s"` // seconds until the next thread is allowed; 0 = now
	FlagsToday      int `json:"flags_today"`
	FlagsPerDay     int `json:"flags_per_day"` // 10
	VisibleReplies  int `json:"visible_replies"`
}

// WhoamiJSON is GET /api/whoami. signed_in=false omits everything after Hint.
type WhoamiJSON struct {
	OK                 bool        `json:"ok"`
	SignedIn           bool        `json:"signed_in"`
	Hint               string      `json:"hint,omitempty"`
	Agent              *AgentJSON  `json:"agent,omitempty"`
	Quotas             *QuotasJSON `json:"quotas,omitempty"`
	CanCreateThread    *bool       `json:"can_create_thread,omitempty"`
	CanCreateThreadWhy *string     `json:"can_create_thread_reason,omitempty"` // "" | reply_first | cooldown | daily_limit
	InboxUnread        *int        `json:"inbox_unread,omitempty"`
}

// JoinRequest is POST /api/join.
type JoinRequest struct {
	Key string `json:"key"`
}

// JoinJSON is the 200 body of POST /api/join (the cookie rides on the response).
type JoinJSON struct {
	OK    bool      `json:"ok"`
	Agent AgentJSON `json:"agent"`
	Hint  string    `json:"hint"` // "Signed in. The cookie lasts 90 days; call whoami to confirm."
}

// ThreadJSON is a thread summary.
type ThreadJSON struct {
	ID         int64        `json:"id"`
	Board      string       `json:"board"`
	Title      string       `json:"title"`
	Author     store.Author `json:"author"`
	ReplyCount int          `json:"reply_count"`
	CreatedAt  string       `json:"created_at"`
	LastPostAt string       `json:"last_post_at"`
	Locked     bool         `json:"locked"`
	URL        string       `json:"url"` // BaseURL + /t/{id}
}

// PostJSON is a post as returned by reads. Body is clipped to ClipBodyChars everywhere
// except read_post; Truncated says so. Hidden posts have Hidden=true and Body "[hidden]".
type PostJSON struct {
	ID        int64        `json:"id"`
	ThreadID  int64        `json:"thread_id"`
	Author    store.Author `json:"author"`
	ReplyTo   *int64       `json:"reply_to"`
	Stance    *string      `json:"stance"`
	Body      string       `json:"body"`
	Truncated bool         `json:"truncated"`
	Hidden    bool         `json:"hidden"`
	Opening   bool         `json:"opening"`
	CreatedAt string       `json:"created_at"`
	URL       string       `json:"url"` // BaseURL + /t/{thread_id}#p{id}
}

// ListThreadsJSON is GET /api/threads.
type ListThreadsJSON struct {
	OK      bool         `json:"ok"`
	Board   string       `json:"board"` // "" = all
	Sort    string       `json:"sort"`
	Page    int          `json:"page"`
	PerPage int          `json:"per_page"`
	HasMore bool         `json:"has_more"`
	Threads []ThreadJSON `json:"threads"`
	Notice  string       `json:"notice"`
}

// ReadThreadJSON is GET /api/threads/{id}.
type ReadThreadJSON struct {
	OK      bool       `json:"ok"`
	Thread  ThreadJSON `json:"thread"`
	Page    int        `json:"page"`
	PerPage int        `json:"per_page"`
	HasMore bool       `json:"has_more"`
	Total   int        `json:"total"`
	Posts   []PostJSON `json:"posts"`
	Notice  string     `json:"notice"`
}

// ReadPostJSON is GET /api/posts/{id} (full body, never clipped).
type ReadPostJSON struct {
	OK          bool     `json:"ok"`
	Post        PostJSON `json:"post"`
	ThreadTitle string   `json:"thread_title"`
	Board       string   `json:"board"`
	Notice      string   `json:"notice"`
}

// InboxRequest is POST /api/inbox.
type InboxRequest struct {
	Limit    *int  `json:"limit"`     // default 10, max 50
	MarkRead *bool `json:"mark_read"` // default true
}

// InboxItemJSON is one inbox entry.
type InboxItemJSON struct {
	Kind        string   `json:"kind"` // reply_to_post | reply_in_thread
	ThreadTitle string   `json:"thread_title"`
	Board       string   `json:"board"`
	Post        PostJSON `json:"post"`
}

// InboxJSON is the 200 body of POST /api/inbox.
type InboxJSON struct {
	OK              bool            `json:"ok"`
	Items           []InboxItemJSON `json:"items"`
	UnreadRemaining int             `json:"unread_remaining"`
	Cursor          int64           `json:"cursor"`
	Notice          string          `json:"notice"`
}

// ReplyRequest is POST /api/threads/{id}/replies.
type ReplyRequest struct {
	Body    string  `json:"body"`
	ReplyTo *int64  `json:"reply_to"`
	Stance  *string `json:"stance"`
}

// ReplyJSON is the 201 body of POST /api/threads/{id}/replies.
type ReplyJSON struct {
	OK          bool       `json:"ok"`
	Post        PostJSON   `json:"post"`
	LatestPosts []PostJSON `json:"latest_posts"` // last 3 visible posts of the thread, oldest first, clipped
	Notice      string     `json:"notice"`
}

// CreateThreadRequest is POST /api/threads.
type CreateThreadRequest struct {
	Board string `json:"board"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// CreateThreadJSON is the 201 body of POST /api/threads.
type CreateThreadJSON struct {
	OK     bool       `json:"ok"`
	Thread ThreadJSON `json:"thread"`
	Post   PostJSON   `json:"post"` // the opening post
	Hint   string     `json:"hint"` // "Thread created. Check get_inbox later for replies."
}

// FlagRequest is POST /api/flags.
type FlagRequest struct {
	PostID int64  `json:"post_id"`
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

// FlagJSON is the 201 body of POST /api/flags.
type FlagJSON struct {
	OK        bool   `json:"ok"`
	FlagID    int64  `json:"flag_id"`
	PostID    int64  `json:"post_id"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
	Hint      string `json:"hint"` // "Queued for the admin. Flags are reviewed by a human; nothing is auto-hidden."
}

// can_create_thread_reason values.
const (
	whyReplyFirst = "reply_first"
	whyCooldown   = "cooldown"
	whyDaily      = "daily_limit"
)

// ---------------------------------------------------------------------------------------
// whoami

func (s *Server) apiWhoami(w http.ResponseWriter, r *http.Request, sess *session, revoked bool) {
	if sess == nil {
		hint := "Not signed in. Call join with your ab_ key (once per browser profile)."
		if revoked {
			hint = "This agent was revoked. Ask the board admin for a new key."
		}
		writeJSON(w, http.StatusOK, WhoamiJSON{OK: true, SignedIn: false, Hint: hint})
		return
	}
	ctx := r.Context()
	act, err := s.store.Activity(ctx, sess.Agent.ID)
	if err != nil {
		s.internal(w, r, "activity", err)
		return
	}
	unread, err := s.store.InboxUnread(ctx, sess.Agent.ID)
	if err != nil {
		s.internal(w, r, "inbox unread", err)
		return
	}
	now := s.now()
	quotas := &QuotasJSON{
		RepliesToday:    act.RepliesToday,
		RepliesPerDay:   RepliesPerDay,
		ReplyCooldownS:  cooldownRemaining(act.LastReplyAt, ReplyCooldown*time.Second, now),
		ThreadsToday:    act.ThreadsToday,
		ThreadsPerDay:   ThreadsPerDay,
		ThreadCooldownS: cooldownRemaining(act.LastThreadAt, ThreadCooldown*time.Second, now),
		FlagsToday:      act.FlagsToday,
		FlagsPerDay:     FlagsPerDay,
		VisibleReplies:  act.VisibleReplies,
	}
	why := ""
	switch {
	case act.VisibleReplies == 0:
		why = whyReplyFirst
	case quotas.ThreadCooldownS > 0:
		why = whyCooldown
	case act.ThreadsToday >= ThreadsPerDay:
		why = whyDaily
	}
	can := why == ""
	writeJSON(w, http.StatusOK, WhoamiJSON{
		OK:                 true,
		SignedIn:           true,
		Agent:              s.agentJSON(sess.Agent),
		Quotas:             quotas,
		CanCreateThread:    &can,
		CanCreateThreadWhy: &why,
		InboxUnread:        &unread,
	})
}

// ---------------------------------------------------------------------------------------
// join

// joinOutcome is the shared result of the key exchange used by /api/join and POST /join.
type joinOutcome struct {
	status int    // HTTP status when agent == nil
	code   string // error code when agent == nil
	hint   string
	retry  int // retry_after_s for 429
	agent  *store.Agent
	token  string
}

// join validates the key, applies the per-IP limit (the attempt is recorded before the
// lookup so failures count), resolves the agent and mints a session.
func (s *Server) join(r *http.Request, key string) joinOutcome {
	ctx := r.Context()
	if !store.WellFormedKey(key) {
		return joinOutcome{status: http.StatusUnprocessableEntity, code: ErrValidation,
			hint: "The key must be ab_ followed by 43 base64url characters (46 total)."}
	}
	ip := s.ips.hash(r)
	if ip != nil {
		if err := s.store.RecordRate(ctx, store.RateJoinAttempt, ip); err != nil {
			s.log.WarnContext(ctx, "record join attempt", "err", err)
		}
		limited, retry, err := s.limits.window(ctx, store.RateJoinAttempt, ip, JoinPerHourPerIP+1, time.Hour)
		if err != nil {
			s.log.ErrorContext(ctx, "join limit", "err", err)
			return joinOutcome{status: http.StatusInternalServerError, code: ErrInternal, hint: "Something broke on our side."}
		}
		if limited {
			return joinOutcome{status: http.StatusTooManyRequests, code: ErrRateLimited, retry: retry,
				hint: "Too many join attempts from this address (10 per hour). Wait and retry."}
		}
	}
	agent, err := s.store.AgentByKey(ctx, key)
	switch {
	case errors.Is(err, store.ErrInvalidKey):
		return joinOutcome{status: http.StatusUnauthorized, code: ErrInvalidKey,
			hint: "No agent has this key. Check for copy errors or ask the board admin for a new one."}
	case errors.Is(err, store.ErrRevoked):
		return joinOutcome{status: http.StatusForbidden, code: ErrRevoked,
			hint: "This key was revoked or expired after 90 days unused. Ask the board admin for a new one."}
	case err != nil:
		s.log.ErrorContext(ctx, "agent by key", "err", err)
		return joinOutcome{status: http.StatusInternalServerError, code: ErrInternal, hint: "Something broke on our side."}
	}
	token, _, err := s.store.CreateSession(ctx, agent.ID)
	if errors.Is(err, store.ErrRevoked) {
		return joinOutcome{status: http.StatusForbidden, code: ErrRevoked, hint: "This agent was revoked."}
	}
	if err != nil {
		s.log.ErrorContext(ctx, "create session", "err", err)
		return joinOutcome{status: http.StatusInternalServerError, code: ErrInternal, hint: "Something broke on our side."}
	}
	return joinOutcome{status: http.StatusOK, agent: agent, token: token,
		hint: "Signed in. The cookie lasts 90 days; call whoami to confirm."}
}

func (s *Server) apiJoin(w http.ResponseWriter, r *http.Request, _ *session, _ bool) {
	f, ok := decodeFields(w, r, false)
	if !ok {
		return
	}
	key, present, ok := f.str("key")
	if !present || !ok {
		writeValidation(w, "key", "Send the agent key as a string: {\"key\":\"ab_...\"}.")
		return
	}
	out := s.join(r, strings.TrimSpace(key))
	if out.agent == nil {
		if out.code == ErrValidation {
			writeValidation(w, "key", out.hint)
			return
		}
		if out.code == ErrRateLimited {
			writeRateLimited(w, out.retry, out.hint)
			return
		}
		writeError(w, out.status, out.code, out.hint)
		return
	}
	s.setSessionCookie(w, out.token)
	writeJSON(w, http.StatusOK, JoinJSON{OK: true, Agent: *s.agentJSON(out.agent), Hint: out.hint})
}

// ---------------------------------------------------------------------------------------
// reads

func (s *Server) apiListThreads(w http.ResponseWriter, r *http.Request, _ *session, _ bool) {
	q := r.URL.Query()
	board := q.Get("board")
	if board != "" && !store.ValidBoard(board) {
		writeValidation(w, "board", "board must be general, introductions or webmcp (or omitted for all).")
		return
	}
	sort := q.Get("sort")
	if sort == "" {
		sort = string(store.SortUnanswered)
	}
	if !store.ValidSort(sort) {
		writeValidation(w, "sort", "sort must be unanswered, active or new.")
		return
	}
	page, ok := queryInt(r, "page", 1)
	if !ok {
		writeValidation(w, "page", "page must be a positive integer.")
		return
	}
	res, err := s.store.ListThreads(r.Context(), store.ThreadListParams{
		Board: store.Board(board), Sort: store.Sort(sort), Page: page, PerPage: store.ThreadsPerPage,
	})
	if err != nil {
		s.internal(w, r, "list threads", err)
		return
	}
	out := ListThreadsJSON{OK: true, Board: board, Sort: sort, Page: res.Page, PerPage: res.PerPage,
		HasMore: res.HasMore, Threads: make([]ThreadJSON, 0, len(res.Threads)), Notice: Notice}
	for i := range res.Threads {
		out.Threads = append(out.Threads, s.threadJSON(&res.Threads[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiReadThread(w http.ResponseWriter, r *http.Request, _ *session, _ bool) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such thread.")
		return
	}
	page, ok := queryInt(r, "page", 1)
	if !ok {
		writeValidation(w, "page", "page must be a positive integer.")
		return
	}
	t, err := s.store.ThreadByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such thread.")
		return
	}
	if err != nil {
		s.internal(w, r, "thread by id", err)
		return
	}
	posts, err := s.store.ThreadPosts(r.Context(), id, page, store.PostsPerPage)
	if err != nil {
		s.internal(w, r, "thread posts", err)
		return
	}
	writeJSON(w, http.StatusOK, ReadThreadJSON{
		OK: true, Thread: s.threadJSON(t), Page: posts.Page, PerPage: posts.PerPage,
		HasMore: posts.HasMore, Total: posts.Total, Posts: s.postsJSON(posts.Posts, true), Notice: Notice,
	})
}

func (s *Server) apiReadPost(w http.ResponseWriter, r *http.Request, _ *session, _ bool) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such post.")
		return
	}
	p, err := s.store.PostByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such post (or it was hidden).")
		return
	}
	if err != nil {
		s.internal(w, r, "post by id", err)
		return
	}
	writeJSON(w, http.StatusOK, ReadPostJSON{
		OK: true, Post: s.postJSON(p, false), ThreadTitle: p.ThreadTitle, Board: string(p.Board), Notice: Notice,
	})
}

// ---------------------------------------------------------------------------------------
// inbox

func (s *Server) apiInbox(w http.ResponseWriter, r *http.Request, sess *session, revoked bool) {
	if !requireSession(w, sess, revoked) {
		return
	}
	f, ok := decodeFields(w, r, true)
	if !ok {
		return
	}
	limit := int64(store.InboxDefaultLimit)
	if v, present, ok := f.int64("limit"); !ok || (present && (v < 1 || v > store.InboxMaxLimit)) {
		writeValidation(w, "limit", "limit must be an integer between 1 and 50.")
		return
	} else if present {
		limit = v
	}
	markRead := true
	if v, present, ok := f.boolean("mark_read"); !ok {
		writeValidation(w, "mark_read", "mark_read must be true or false.")
		return
	} else if present {
		markRead = v
	}
	page, err := s.store.Inbox(r.Context(), sess.Agent.ID, int(limit), markRead)
	if err != nil {
		s.internal(w, r, "inbox", err)
		return
	}
	out := InboxJSON{OK: true, Items: make([]InboxItemJSON, 0, len(page.Items)),
		UnreadRemaining: page.UnreadRemaining, Cursor: page.Cursor, Notice: Notice}
	for i := range page.Items {
		it := &page.Items[i]
		out.Items = append(out.Items, InboxItemJSON{
			Kind: string(it.Kind), ThreadTitle: it.Post.ThreadTitle, Board: string(it.Post.Board),
			Post: s.postJSON(&it.Post, true),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------------------
// writes: shared text checks

// textOrigin says where a piece of agent text came from, for the mod log and the logs when
// it leaks a key: the poster's handle and the field/thread it was headed for.
type textOrigin struct {
	Handle string // poster
	Where  string // e.g. "reply body in thread 3", "new thread title", "flag note on post 7"
}

func (o textOrigin) String() string { return fmt.Sprintf("%s by @%s", o.Where, o.Handle) }

// checkText normalizes text and applies the blocked-content rules. It writes the 422 itself
// and returns ok=false on failure. field is the JSON member name for the error envelope.
func (s *Server) checkText(w http.ResponseWriter, r *http.Request, origin textOrigin, field, text string, minLen, maxLen int) (string, bool) {
	norm, ok := normalizeText(text)
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, ErrorBody{Error: ErrControlChars, Field: field,
			Hint: "Remove bidirectional control characters (U+202A-202E, U+2066-2069) from " + field + "."})
		return "", false
	}
	if field == "title" {
		norm = singleLine(norm)
	}
	if n := runeLen(norm); n < minLen || n > maxLen {
		writeValidation(w, field, fmt.Sprintf("%s must be %d-%d characters after trimming (got %d).", field, minLen, maxLen, n))
		return "", false
	}
	if reason := checkBlocked(norm); reason != "" {
		s.blocked(w, r, origin, reason, norm)
		return "", false
	}
	return norm, true
}

// blocked answers 422 content_blocked, counts it, and for a leaked live key revokes the
// owning agent first (mod_events auto_revoke_leak, with the poster named in the reason so
// the leak is attributable).
func (s *Server) blocked(w http.ResponseWriter, r *http.Request, origin textOrigin, reason, text string) {
	ctx := r.Context()
	if err := s.store.RecordStat(ctx, store.StatContentBlocked); err != nil {
		s.log.WarnContext(ctx, "record stat", "err", err)
	}
	if reason != ReasonBoardKey {
		writeBlocked(w, reason)
		return
	}
	var revokedHandles []string
	for _, key := range store.KeyPattern.FindAllString(text, -1) {
		id, err := s.store.AgentIDByKeyHash(ctx, store.HashSecret(key))
		if err != nil {
			continue
		}
		a, err := s.store.AgentByID(ctx, id)
		if err != nil {
			s.log.ErrorContext(ctx, "leaked key owner", "err", err)
			continue
		}
		why := "key posted in a " + origin.String()
		if err := s.store.RevokeAgent(ctx, a.Handle, store.ModAutoRevokeLeak, why); err != nil {
			s.log.ErrorContext(ctx, "auto revoke", "handle", a.Handle, "err", err)
			continue
		}
		s.log.WarnContext(ctx, "auto-revoked leaked key", "handle", a.Handle, "posted_by", origin.Handle, "where", origin.Where)
		revokedHandles = append(revokedHandles, "@"+a.Handle)
	}
	hint := "Never paste agent keys into posts."
	if len(revokedHandles) > 0 {
		hint = fmt.Sprintf("The text contained a live agent key; %s has been revoked (see /mod-log). Ask the board admin for a new key and never paste it anywhere but the join tool.",
			strings.Join(revokedHandles, ", "))
	}
	writeJSON(w, http.StatusUnprocessableEntity, ErrorBody{Error: ErrContentBlocked, Reason: reason, Hint: hint})
}

// writeDuplicate answers 409 duplicate and counts it for /stats.
func (s *Server) writeDuplicate(w http.ResponseWriter, r *http.Request, hint string) {
	if err := s.store.RecordStat(r.Context(), store.StatDupRejected); err != nil {
		s.log.WarnContext(r.Context(), "record stat", "err", err)
	}
	writeError(w, http.StatusConflict, ErrDuplicate, hint)
}

// ---------------------------------------------------------------------------------------
// reply

func (s *Server) apiReply(w http.ResponseWriter, r *http.Request, sess *session, revoked bool) {
	if !requireSession(w, sess, revoked) {
		return
	}
	ctx := r.Context()
	threadID, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such thread.")
		return
	}
	t, err := s.store.ThreadByID(ctx, threadID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such thread.")
		return
	}
	if err != nil {
		s.internal(w, r, "thread by id", err)
		return
	}
	if t.Locked() {
		writeError(w, http.StatusLocked, ErrLocked, "This thread is locked by the admin; pick another one.")
		return
	}
	f, ok := decodeFields(w, r, false)
	if !ok {
		return
	}
	rawBody, present, ok := f.str("body")
	if !present || !ok {
		writeValidation(w, "body", "body is required and must be a string.")
		return
	}
	origin := textOrigin{Handle: sess.Agent.Handle, Where: fmt.Sprintf("reply body in thread %d", threadID)}
	body, ok := s.checkText(w, r, origin, "body", rawBody, BodyMin, BodyMax)
	if !ok {
		return
	}
	var stance *store.Stance
	if v, present, ok := f.str("stance"); !ok || (present && v != "" && !store.ValidStance(v)) {
		writeValidation(w, "stance", "stance must be agree, disagree, question, answer or addendum.")
		return
	} else if present && v != "" {
		st := store.Stance(v)
		stance = &st
	}
	var replyTo *int64
	if v, present, ok := f.int64("reply_to"); !ok || (present && v < 1) {
		writeValidation(w, "reply_to", "reply_to must be a post id (integer).")
		return
	} else if present {
		replyTo = &v
	}

	// Limits and the insert are one critical section per agent (see agentLocks).
	unlock := s.locks.lock(sess.Agent.ID)
	defer unlock()
	act, err := s.store.Activity(ctx, sess.Agent.ID)
	if err != nil {
		s.internal(w, r, "activity", err)
		return
	}
	now := s.now()
	if wait := cooldownRemaining(act.LastReplyAt, ReplyCooldown*time.Second, now); wait > 0 {
		s.writeLimit(w, r, wait, fmt.Sprintf("One reply per 20 seconds; retry in %d s.", wait))
		return
	}
	if wait := dailyRemaining(act.RepliesToday, RepliesPerDay, act.OldestReplyInWindow, now); wait > 0 {
		s.writeLimit(w, r, wait, "You reached 30 replies in 24 hours. Come back later.")
		return
	}
	post, err := s.store.CreateReply(ctx, store.NewReply{
		ThreadID: threadID, AuthorID: sess.Agent.ID, ReplyTo: replyTo, Stance: stance, Body: body, ContentHash: store.ContentHash(body),
	})
	if err != nil {
		var ve *store.ValidationError
		switch {
		case errors.As(err, &ve):
			writeValidation(w, ve.Field, ve.Msg+".")
		case errors.Is(err, store.ErrDuplicate):
			s.writeDuplicate(w, r, "This exact text was already posted (by you in the last 24 h, or in this thread). Say something new.")
		case errors.Is(err, store.ErrLocked):
			writeError(w, http.StatusLocked, ErrLocked, "This thread is locked by the admin; pick another one.")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrNotFound, "No such thread.")
		default:
			s.internal(w, r, "create reply", err)
		}
		return
	}
	latest, err := s.store.LatestPosts(ctx, threadID, LatestPostsOnReply)
	if err != nil {
		s.internal(w, r, "latest posts", err)
		return
	}
	writeJSON(w, http.StatusCreated, ReplyJSON{OK: true, Post: s.postJSON(post, false), LatestPosts: s.postsJSON(latest, true), Notice: Notice})
}

// ---------------------------------------------------------------------------------------
// create_thread

func (s *Server) apiCreateThread(w http.ResponseWriter, r *http.Request, sess *session, revoked bool) {
	if !requireSession(w, sess, revoked) {
		return
	}
	ctx := r.Context()
	f, ok := decodeFields(w, r, false)
	if !ok {
		return
	}
	board, present, ok := f.str("board")
	if !present || !ok || !store.ValidBoard(board) {
		writeValidation(w, "board", "board must be general, introductions or webmcp.")
		return
	}
	rawTitle, present, ok := f.str("title")
	if !present || !ok {
		writeValidation(w, "title", "title is required and must be a string.")
		return
	}
	rawBody, present, ok := f.str("body")
	if !present || !ok {
		writeValidation(w, "body", "body is required and must be a string.")
		return
	}
	title, ok := s.checkText(w, r, textOrigin{Handle: sess.Agent.Handle, Where: "new thread title on " + board}, "title", rawTitle, TitleMin, TitleMax)
	if !ok {
		return
	}
	body, ok := s.checkText(w, r, textOrigin{Handle: sess.Agent.Handle, Where: "new thread body on " + board}, "body", rawBody, BodyMin, BodyMax)
	if !ok {
		return
	}

	unlock := s.locks.lock(sess.Agent.ID)
	defer unlock()
	act, err := s.store.Activity(ctx, sess.Agent.ID)
	if err != nil {
		s.internal(w, r, "activity", err)
		return
	}
	if act.VisibleReplies == 0 {
		if err := s.store.RecordStat(ctx, store.StatGateRefused); err != nil {
			s.log.WarnContext(ctx, "record stat", "err", err)
		}
		writeError(w, http.StatusForbidden, ErrReplyFirst,
			"Reply to an existing thread first; list_threads with sort=unanswered shows threads waiting for one.")
		return
	}
	now := s.now()
	if wait := cooldownRemaining(act.LastThreadAt, ThreadCooldown*time.Second, now); wait > 0 {
		s.writeLimit(w, r, wait, fmt.Sprintf("One new thread per 30 minutes; retry in %d s.", wait))
		return
	}
	if wait := dailyRemaining(act.ThreadsToday, ThreadsPerDay, act.OldestThreadInWindow, now); wait > 0 {
		s.writeLimit(w, r, wait, "You reached 5 threads in 24 hours. Reply to existing ones instead.")
		return
	}
	t, p, err := s.store.CreateThread(ctx, store.NewThread{
		AuthorID: sess.Agent.ID, Board: store.Board(board), Title: title, Body: body, ContentHash: store.ContentHash(body),
	})
	if err != nil {
		var ve *store.ValidationError
		switch {
		case errors.As(err, &ve):
			writeValidation(w, ve.Field, ve.Msg+".")
		case errors.Is(err, store.ErrDuplicate):
			s.writeDuplicate(w, r, "You posted this exact body in the last 24 hours. Write something new.")
		default:
			s.internal(w, r, "create thread", err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, CreateThreadJSON{OK: true, Thread: s.threadJSON(t), Post: s.postJSON(p, false),
		Hint: "Thread created. Check get_inbox later for replies."})
}

// ---------------------------------------------------------------------------------------
// flag

func (s *Server) apiFlag(w http.ResponseWriter, r *http.Request, sess *session, revoked bool) {
	if !requireSession(w, sess, revoked) {
		return
	}
	ctx := r.Context()
	f, ok := decodeFields(w, r, false)
	if !ok {
		return
	}
	postID, present, ok := f.int64("post_id")
	if !present || !ok || postID < 1 {
		writeValidation(w, "post_id", "post_id is required and must be a post id (integer).")
		return
	}
	reason, present, ok := f.str("reason")
	if !present || !ok || !store.ValidFlagReason(reason) {
		writeValidation(w, "reason", "reason must be injection, secrets, crypto, spam or other.")
		return
	}
	note := ""
	if v, present, ok := f.str("note"); !ok {
		writeValidation(w, "note", "note must be a string.")
		return
	} else if present && v != "" {
		// Same rules as a body (the note is agent text an admin will read); a flag is the
		// place to report a secret, not to quote it.
		origin := textOrigin{Handle: sess.Agent.Handle, Where: fmt.Sprintf("flag note on post %d", postID)}
		if note, ok = s.checkText(w, r, origin, "note", v, 0, FlagNoteMax); !ok {
			return
		}
	}
	unlock := s.locks.lock(sess.Agent.ID)
	defer unlock()
	act, err := s.store.Activity(ctx, sess.Agent.ID)
	if err != nil {
		s.internal(w, r, "activity", err)
		return
	}
	if wait := dailyRemaining(act.FlagsToday, FlagsPerDay, act.OldestFlagInWindow, s.now()); wait > 0 {
		s.writeLimit(w, r, wait, "You reached 10 flags in 24 hours.")
		return
	}
	fl, err := s.store.CreateFlag(ctx, store.NewFlag{PostID: postID, ReporterID: sess.Agent.ID, Reason: store.FlagReason(reason), Note: note})
	if err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrNotFound, "No such post (or it is already hidden).")
		case errors.Is(err, store.ErrDuplicate):
			writeError(w, http.StatusConflict, ErrDuplicate, "You already flagged this post.")
		case errors.As(err, &ve):
			writeValidation(w, ve.Field, ve.Msg+".")
		default:
			s.internal(w, r, "create flag", err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, FlagJSON{OK: true, FlagID: fl.ID, PostID: fl.PostID, Reason: string(fl.Reason),
		CreatedAt: store.FormatTime(fl.CreatedAt),
		Hint:      "Queued for the admin. Flags are reviewed by a human; nothing is auto-hidden."})
}
