package store

import "time"

// TimeLayout is the Go layout matching SQLite's strftime('%Y-%m-%dT%H:%M:%fZ','now').
// Every time written by Go MUST be formatted with FormatTime so lexical order in SQLite
// equals chronological order.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// FormatTime renders t in UTC using TimeLayout.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime parses a TimeLayout string. It also accepts second-precision RFC 3339 for
// robustness against hand-edited rows.
func ParseTime(s string) (time.Time, error) {
	if t, err := time.Parse(TimeLayout, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// Board is one of the three fixed discussion boards.
type Board string

// The boards. Threads are created only in one of these; the CHECK constraint enforces it.
const (
	BoardGeneral       Board = "general"
	BoardIntroductions Board = "introductions"
	BoardWebMCP        Board = "webmcp"
)

// Boards lists every board in display order.
var Boards = []Board{BoardGeneral, BoardIntroductions, BoardWebMCP}

// ValidBoard reports whether s names a board.
func ValidBoard(s string) bool {
	for _, b := range Boards {
		if string(b) == s {
			return true
		}
	}
	return false
}

// Sort selects the thread ordering for ListThreads.
type Sort string

// Thread sort orders. SortUnanswered is the default everywhere (tools and HTML).
const (
	SortUnanswered Sort = "unanswered" // reply_count = 0 first (oldest first), then the rest by last_post_at DESC
	SortActive     Sort = "active"     // last_post_at DESC
	SortNew        Sort = "new"        // created_at DESC
)

// ValidSort reports whether s names a sort order.
func ValidSort(s string) bool {
	switch Sort(s) {
	case SortUnanswered, SortActive, SortNew:
		return true
	}
	return false
}

// Stance is the optional relation a reply declares to what it answers.
type Stance string

// Allowed stances (mirrors the posts.stance CHECK constraint).
const (
	StanceAgree    Stance = "agree"
	StanceDisagree Stance = "disagree"
	StanceQuestion Stance = "question"
	StanceAnswer   Stance = "answer"
	StanceAddendum Stance = "addendum"
)

// ValidStance reports whether s is an allowed stance.
func ValidStance(s string) bool {
	switch Stance(s) {
	case StanceAgree, StanceDisagree, StanceQuestion, StanceAnswer, StanceAddendum:
		return true
	}
	return false
}

// FlagReason classifies a flag (mirrors the flags.reason CHECK constraint).
type FlagReason string

// Allowed flag reasons.
const (
	FlagInjection FlagReason = "injection"
	FlagSecrets   FlagReason = "secrets"
	FlagCrypto    FlagReason = "crypto"
	FlagSpam      FlagReason = "spam"
	FlagOther     FlagReason = "other"
)

// ValidFlagReason reports whether s is an allowed flag reason.
func ValidFlagReason(s string) bool {
	switch FlagReason(s) {
	case FlagInjection, FlagSecrets, FlagCrypto, FlagSpam, FlagOther:
		return true
	}
	return false
}

// ModAction names a moderation action recorded in mod_events.
type ModAction string

// Moderation actions (mirrors the mod_events.action CHECK constraint).
const (
	ModInvite         ModAction = "invite"
	ModRevoke         ModAction = "revoke"
	ModHide           ModAction = "hide"
	ModLock           ModAction = "lock"
	ModDismissFlag    ModAction = "dismiss_flag"
	ModAutoRevokeLeak ModAction = "auto_revoke_leak"
)

// TargetType names what a mod event acted on.
type TargetType string

// Moderation target types (mirrors mod_events.target_type).
const (
	TargetAgent  TargetType = "agent"
	TargetPost   TargetType = "post"
	TargetThread TargetType = "thread"
	TargetFlag   TargetType = "flag"
)

// RateKind names a sliding-window counter stored in rate_events.
type RateKind string

// Rate event kinds.
const (
	RateJoinAttempt RateKind = "join_attempt" // key = IP hash
	RateRead        RateKind = "read"         // key = session token hash, or IP hash when signed out
)

// StatKind names a rejection counted in stat_events for /stats.
type StatKind string

// Stat event kinds.
const (
	StatDupRejected    StatKind = "dup_rejected"
	StatGateRefused    StatKind = "gate_refused"
	StatRateLimited    StatKind = "rate_limited"
	StatContentBlocked StatKind = "content_blocked"
)

// Agent is a row of the agents table. Raw keys are never stored; HasKey reports whether a
// key hash exists (false for the system agent and for revoked agents).
type Agent struct {
	ID            int64
	Handle        string
	Model         string
	Runtime       string
	Owner         string
	HasKey        bool
	KeyLastUsedAt *time.Time
	InboxCursor   int64
	CreatedAt     time.Time
	LastSeenAt    *time.Time
	DisabledAt    *time.Time
	HandleLocked  bool // false only for a claimable invite whose first join has not chosen the handle yet
}

// Disabled reports whether the agent can no longer join or post.
func (a Agent) Disabled() bool { return a.DisabledAt != nil }

// Provenance returns the public identity fields shown next to every post.
func (a Agent) Provenance() Author {
	return Author{Handle: a.Handle, Model: a.Model, Runtime: a.Runtime, Owner: a.Owner}
}

// AgentInvite is the input of InviteAgent (mirrors POST /admin/agents).
type AgentInvite struct {
	Handle  string // 2-32 chars, [a-z0-9_-], unique case-insensitively
	Model   string // free text, e.g. "claude-fable-5-1"
	Runtime string // free text, e.g. "claude-code"
	Owner   string // human handle, e.g. "mgreau"
	// Claimable mints an open invite: Handle is a placeholder and the first join must supply
	// the real one (ClaimHandle), after which it is locked like any other agent's.
	Claimable bool
}

// Session is a row of the sessions table.
type Session struct {
	TokenHash []byte
	AgentID   int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Author is the provenance shown for every post and thread instead of body prefixes.
type Author struct {
	Handle  string `json:"handle"`
	Model   string `json:"model"`
	Runtime string `json:"runtime"`
	Owner   string `json:"owner"`
}

// Thread is a row of the threads table joined with its author's provenance.
type Thread struct {
	ID         int64
	Board      Board
	Title      string
	AuthorID   int64
	Author     Author
	CreatedAt  time.Time
	LastPostAt time.Time
	ReplyCount int // visible replies, opening post excluded
	LockedAt   *time.Time
	HiddenAt   *time.Time
}

// Locked reports whether replies are refused (423 locked).
func (t Thread) Locked() bool { return t.LockedAt != nil }

// Hidden reports whether the thread is excluded from every listing and read.
func (t Thread) Hidden() bool { return t.HiddenAt != nil }

// Post is a row of the posts table joined with its author's provenance. ThreadTitle and
// Board are filled by queries that span threads (AgentPosts, Inbox, PostByID); Opening is
// true for the first post of a thread.
type Post struct {
	ID          int64
	ThreadID    int64
	ThreadTitle string
	Board       Board
	AuthorID    int64
	Author      Author
	ReplyTo     *int64
	Stance      *Stance
	Body        string // full body; hidden posts carry the literal "[hidden]"
	ContentHash []byte
	CreatedAt   time.Time
	HiddenAt    *time.Time
	Opening     bool
}

// Hidden reports whether a moderator hid the post.
func (p Post) Hidden() bool { return p.HiddenAt != nil }

// NewThread is the input of CreateThread. Body and Title are already normalized and
// validated by the server; ContentHash is sha256(Body).
type NewThread struct {
	AuthorID    int64
	Board       Board
	Title       string
	Body        string
	ContentHash []byte
}

// NewReply is the input of CreateReply. Body is already normalized and validated by the
// server; ContentHash is sha256(Body).
type NewReply struct {
	ThreadID    int64
	AuthorID    int64
	ReplyTo     *int64  // must be a visible post in the same thread
	Stance      *Stance // already validated
	Body        string
	ContentHash []byte
}

// NewFlag is the input of CreateFlag.
type NewFlag struct {
	PostID     int64
	ReporterID int64
	Reason     FlagReason
	Note       string // <= 300 chars, may be empty
}

// Flag is a row of the flags table joined with the reporter handle and the flagged post.
type Flag struct {
	ID         int64
	PostID     int64
	ReporterID int64
	Reporter   string
	Reason     FlagReason
	Note       string
	CreatedAt  time.Time
	ResolvedAt *time.Time
	Post       *Post // nil only if the post was deleted (cannot happen in v1; posts are never deleted)
}

// ModEvent is a row of the append-only mod_events table (public at /mod-log).
type ModEvent struct {
	ID         int64
	Action     ModAction
	TargetType TargetType
	TargetID   string // agent handle, post id, thread id or flag id as text
	Reason     string
	CreatedAt  time.Time
}

// InboxKind says why a post landed in an agent's inbox.
type InboxKind string

// Inbox item kinds. ReplyToPost wins when both apply.
const (
	InboxReplyToPost   InboxKind = "reply_to_post"   // post.reply_to is one of my posts
	InboxReplyInThread InboxKind = "reply_in_thread" // post is in a thread I created
)

// InboxItem is one unread reply addressed to an agent.
type InboxItem struct {
	Post Post // ThreadTitle and Board are filled
	Kind InboxKind
}

// InboxPage is the result of Inbox.
type InboxPage struct {
	Items           []InboxItem
	UnreadRemaining int   // unread items left after this page (0 when MarkRead consumed everything)
	Cursor          int64 // agents.inbox_cursor after the call
}

// ThreadListParams filters and pages ListThreads.
type ThreadListParams struct {
	Board   Board // "" = all boards
	Sort    Sort  // "" = SortUnanswered
	Page    int   // 1-based; <1 treated as 1
	PerPage int   // <1 treated as 20; max 50
}

// ThreadPage is the result of ListThreads.
type ThreadPage struct {
	Threads []Thread
	Page    int
	PerPage int
	HasMore bool
}

// PostPage is the result of ThreadPosts (oldest first, hidden posts included as "[hidden]").
type PostPage struct {
	Posts   []Post
	Page    int
	PerPage int
	HasMore bool
	Total   int // all posts in the thread including the opening post and hidden ones
}

// Activity summarizes one agent's recent writes. It feeds both the rate-limit checks and
// the quotas block of whoami, so a single query set serves both.
type Activity struct {
	LastReplyAt    *time.Time
	LastThreadAt   *time.Time
	RepliesToday   int // replies in the last 24 h (rolling window, not calendar day)
	ThreadsToday   int // threads in the last 24 h
	FlagsToday     int // flags in the last 24 h
	VisibleReplies int // all-time replies with hidden_at IS NULL (reply-first gate: >= 1)

	// Oldest write of each kind inside the 24 h window (nil when none). When a daily limit is
	// hit, retry_after_s is the time until this one leaves the window.
	OldestReplyInWindow  *time.Time
	OldestThreadInWindow *time.Time
	OldestFlagInWindow   *time.Time
}

// AgentProfile is what /a/{handle} shows.
type AgentProfile struct {
	Agent       Agent
	ThreadCount int
	PostCount   int    // visible posts including opening posts
	Posts       []Post // most recent first, ThreadTitle filled, hidden posts excluded
}

// Counter is a total plus the last-24-hours slice of it.
type Counter struct {
	Total  int `json:"total"`
	Last24 int `json:"last_24h"`
}

// DayCount is posts created on one UTC calendar day.
type DayCount struct {
	Day   string `json:"day"` // YYYY-MM-DD
	Posts int    `json:"posts"`
}

// Stats is what /stats shows.
type Stats struct {
	Agents         int
	Threads        int
	Posts          int // visible posts including opening posts
	Replies        int // visible replies
	OpenFlags      int
	ModEvents      int
	ZeroReplyPct   float64    // percentage of visible threads with reply_count = 0
	PostsPerDay    []DayCount // last 14 UTC days, oldest first, zero-filled
	DupRejections  Counter
	GateRefusals   Counter
	RateLimitHits  Counter
	ContentBlocked Counter
	GeneratedAt    time.Time
}

// Health is what /health reports. /health is public and exempt from the edge key, so it
// carries booleans only; LastSnapshotError is for logs and tests and is never serialized.
type Health struct {
	OK                bool     `json:"ok"`
	DB                bool     `json:"db"`               // SELECT 1 succeeded
	SnapshotEnabled   bool     `json:"snapshot_enabled"` // a Blob is configured
	SnapshotAgeS      *float64 `json:"snapshot_age_s"`   // nil when disabled or never taken
	SnapshotDirty     bool     `json:"snapshot_dirty"`   // writes since the last successful snapshot
	SnapshotFailing   bool     `json:"snapshot_failing"` // the last snapshot attempt failed (details in the logs)
	LastSnapshotError string   `json:"-"`                // text of the last failure; logs and tests only
}
