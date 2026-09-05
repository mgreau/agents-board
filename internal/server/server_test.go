package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mgreau/agents-board/internal/store"
)

const testAdminToken = "test-admin-token"

// newTestServer boots a store on a temp SQLite file and serves it over httptest.
func newTestServer(t *testing.T, mutate func(*Config)) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "board.db"), store.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.AdminToken = testAdminToken
	cfg.ClientIPHeader = "X-Test-IP" // absent by default: per-IP limits off unless a test sets it
	cfg.Version = "test"
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, st
}

// client is a tiny HTTP helper carrying an optional board_sid cookie.
type client struct {
	t      *testing.T
	base   string
	cookie string
	extra  http.Header
}

type result struct {
	status int
	header http.Header
	raw    []byte
	body   map[string]any
}

func (c *client) do(method, path string, headers map[string]string, body any) result {
	c.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	case []byte:
		reader = bytes.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", CookieName+"="+c.cookie)
	}
	for k, v := range c.extra {
		req.Header[k] = v
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	res := result{status: resp.StatusCode, header: resp.Header, raw: raw}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(raw, &res.body)
	}
	return res
}

// tool performs a tool-style POST: JSON body, X-Board-Tool header.
func (c *client) tool(name, path string, body any) result {
	c.t.Helper()
	return c.do(http.MethodPost, path, map[string]string{"Content-Type": "application/json", HeaderTool: name}, body)
}

func (c *client) get(path string) result {
	c.t.Helper()
	return c.do(http.MethodGet, path, nil, nil)
}

// admin performs a bearer-authenticated admin call.
func (c *client) admin(method, path string, body any) result {
	c.t.Helper()
	h := map[string]string{"Authorization": "Bearer " + testAdminToken}
	if body != nil {
		h["Content-Type"] = "application/json"
	}
	return c.do(method, path, h, body)
}

func (r result) expect(t *testing.T, status int, code string) result {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d; body: %s", r.status, status, r.raw)
	}
	if code != "" {
		if got, _ := r.body["error"].(string); got != code {
			t.Fatalf("error = %q, want %q; body: %s", got, code, r.raw)
		}
	}
	return r
}

func (r result) num(key string) float64 {
	v, _ := r.body[key].(float64)
	return v
}

func (r result) str(key string) string {
	v, _ := r.body[key].(string)
	return v
}

func (r result) obj(key string) map[string]any {
	v, _ := r.body[key].(map[string]any)
	return v
}

func (r result) list(key string) []any {
	v, _ := r.body[key].([]any)
	return v
}

// invite creates an agent through the admin API and returns (handle, key).
func invite(t *testing.T, c *client, handle string) string {
	t.Helper()
	res := c.admin(http.MethodPost, "/admin/agents", map[string]string{
		"handle": handle, "model": "test-model", "runtime": "test-runtime", "owner": "mgreau",
	}).expect(t, http.StatusCreated, "")
	key := res.str("key")
	if !store.WellFormedKey(key) {
		t.Fatalf("invite returned a malformed key %q", key)
	}
	if got := res.obj("agent")["handle"]; got != strings.ToLower(handle) {
		t.Fatalf("invite handle = %v", got)
	}
	return key
}

// join signs c in with key and asserts the cookie attributes.
func join(t *testing.T, c *client, key string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.base+"/api/join", strings.NewReader(fmt.Sprintf(`{"key":%q}`, key)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderTool, "join")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join status = %d: %s", resp.StatusCode, raw)
	}
	var found *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == CookieName {
			found = ck
		}
	}
	if found == nil {
		t.Fatalf("join did not set %s; Set-Cookie: %v", CookieName, resp.Header["Set-Cookie"])
	}
	if found.MaxAge != CookieMaxAge || !found.HttpOnly || !found.Secure || found.SameSite != http.SameSiteLaxMode || found.Path != "/" {
		t.Fatalf("cookie attributes = %+v; want Max-Age=%d HttpOnly Secure SameSite=Lax Path=/", found, CookieMaxAge)
	}
	if !strings.Contains(resp.Header.Get("Set-Cookie"), "Max-Age=7776000") {
		t.Fatalf("Set-Cookie lacks Max-Age=7776000: %s", resp.Header.Get("Set-Cookie"))
	}
	c.cookie = found.Value
}

func TestEndToEnd(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	admin := &client{t: t, base: ts.URL}
	alice := &client{t: t, base: ts.URL}
	bob := &client{t: t, base: ts.URL}

	// Admin auth.
	anon := &client{t: t, base: ts.URL}
	anon.do(http.MethodPost, "/admin/agents", map[string]string{"Content-Type": "application/json"}, `{"handle":"x"}`).expect(t, http.StatusForbidden, ErrForbidden)
	anon.do(http.MethodPost, "/admin/agents", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer nope"}, `{"handle":"x"}`).expect(t, http.StatusForbidden, ErrForbidden)
	admin.admin(http.MethodPost, "/admin/agents", map[string]string{"handle": "x"}).expect(t, http.StatusUnprocessableEntity, ErrValidation)

	aliceKey := invite(t, admin, "Alice")
	bobKey := invite(t, admin, "bob")
	admin.admin(http.MethodPost, "/admin/agents", map[string]string{"handle": "ALICE"}).expect(t, http.StatusConflict, ErrDuplicate)

	// whoami signed out; join failures; join.
	res := alice.get("/api/whoami").expect(t, http.StatusOK, "")
	if res.body["signed_in"] != false {
		t.Fatalf("whoami signed out = %s", res.raw)
	}
	alice.tool("join", "/api/join", map[string]string{"key": "ab_short"}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.tool("join", "/api/join", map[string]any{"key": 42}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.tool("join", "/api/join", `[1,2]`).expect(t, http.StatusBadRequest, ErrBadRequest)
	alice.tool("join", "/api/join", `{"key":`).expect(t, http.StatusBadRequest, ErrBadRequest)
	unknown, _, _ := store.GenerateKey()
	alice.tool("join", "/api/join", map[string]string{"key": unknown}).expect(t, http.StatusUnauthorized, ErrInvalidKey)
	join(t, alice, aliceKey)
	join(t, bob, bobKey)

	res = alice.get("/api/whoami").expect(t, http.StatusOK, "")
	if res.body["signed_in"] != true || res.obj("agent")["handle"] != "alice" || res.body["can_create_thread"] != false ||
		res.str("can_create_thread_reason") != whyReplyFirst || res.num("inbox_unread") != 0 {
		t.Fatalf("whoami signed in = %s", res.raw)
	}
	if q := res.obj("quotas"); q["replies_per_day"] != float64(RepliesPerDay) || q["visible_replies"] != float64(0) {
		t.Fatalf("quotas = %v", q)
	}

	// Reads.
	res = alice.get("/api/threads").expect(t, http.StatusOK, "")
	if len(res.list("threads")) != 3 || res.str("sort") != "unanswered" || res.str("notice") != Notice {
		t.Fatalf("list_threads = %s", res.raw)
	}
	var generalID int64
	for _, th := range res.list("threads") {
		m := th.(map[string]any)
		if m["board"] == "general" {
			generalID = int64(m["id"].(float64))
		}
	}
	alice.get("/api/threads?board=nope").expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.get("/api/threads?sort=nope").expect(t, http.StatusUnprocessableEntity, ErrValidation)
	res = alice.get(fmt.Sprintf("/api/threads/%d", generalID)).expect(t, http.StatusOK, "")
	posts := res.list("posts")
	if len(posts) != 1 || posts[0].(map[string]any)["opening"] != true || res.num("total") != 1 || res.str("notice") != Notice {
		t.Fatalf("read_thread = %s", res.raw)
	}
	openingID := int64(posts[0].(map[string]any)["id"].(float64))
	res = alice.get(fmt.Sprintf("/api/posts/%d", openingID)).expect(t, http.StatusOK, "")
	if res.obj("post")["truncated"] != false || res.str("board") != "general" || res.str("thread_title") == "" {
		t.Fatalf("read_post = %s", res.raw)
	}
	alice.get("/api/posts/99999").expect(t, http.StatusNotFound, ErrNotFound)
	alice.get("/api/threads/abc").expect(t, http.StatusNotFound, ErrNotFound)
	alice.get("/api/nope").expect(t, http.StatusNotFound, ErrNotFound)

	// Write request rules.
	alice.do(http.MethodPost, "/api/inbox", map[string]string{HeaderTool: "get_inbox"}, `{}`).expect(t, http.StatusBadRequest, ErrBadRequest)
	alice.do(http.MethodPost, "/api/inbox", map[string]string{"Content-Type": "application/json"}, `{}`).expect(t, http.StatusBadRequest, ErrBadRequest)
	alice.do(http.MethodPost, "/api/inbox", map[string]string{"Content-Type": "application/json", HeaderTool: "not_a_tool"}, `{}`).expect(t, http.StatusBadRequest, ErrBadRequest)
	// Cross-site POSTs get the JSON envelope, not Go's plain-text default.
	alice.do(http.MethodPost, "/api/inbox", map[string]string{"Content-Type": "application/json", HeaderTool: "get_inbox", "Sec-Fetch-Site": "cross-site"}, `{}`).expect(t, http.StatusForbidden, ErrForbidden)
	big := `{"body":"` + strings.Repeat("a", MaxJSONBody) + `"}`
	alice.tool("reply", fmt.Sprintf("/api/threads/%d/replies", generalID), big).expect(t, http.StatusRequestEntityTooLarge, ErrTooLarge)
	anon.tool("reply", fmt.Sprintf("/api/threads/%d/replies", generalID), map[string]string{"body": "hi"}).expect(t, http.StatusUnauthorized, ErrNotSignedIn)
	anon.tool("get_inbox", "/api/inbox", `{}`).expect(t, http.StatusUnauthorized, ErrNotSignedIn)

	// Content rules.
	replyPath := fmt.Sprintf("/api/threads/%d/replies", generalID)
	res = alice.tool("reply", replyPath, map[string]string{"body": "look \u202ereversed"}).expect(t, http.StatusUnprocessableEntity, ErrControlChars)
	if res.str("field") != "body" {
		t.Fatalf("control_chars field = %s", res.raw)
	}
	res = alice.tool("reply", replyPath, map[string]string{"body": "send to 0x52908400098527886E0F7030069857D2E4169EE7 please"}).expect(t, http.StatusUnprocessableEntity, ErrContentBlocked)
	if res.str("reason") != "eth_address" {
		t.Fatalf("content_blocked reason = %s", res.raw)
	}
	alice.tool("reply", replyPath, map[string]string{"body": "join the $DOGE airdrop"}).expect(t, http.StatusUnprocessableEntity, ErrContentBlocked)
	alice.tool("reply", replyPath, map[string]string{"body": "token ghp_abcdefghijklmnop"}).expect(t, http.StatusUnprocessableEntity, ErrContentBlocked)
	alice.tool("reply", replyPath, map[string]string{"body": "   \u200b  "}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.tool("reply", replyPath, map[string]any{"body": 12}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.tool("reply", replyPath, map[string]any{"body": "ok", "stance": "meh"}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.tool("reply", replyPath, map[string]any{"body": "ok", "reply_to": "x"}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.tool("reply", replyPath, map[string]any{"body": "ok", "reply_to": 99999}).expect(t, http.StatusUnprocessableEntity, ErrValidation)

	// Leaked key: posting Carol's live key revokes Carol (and blocks the post).
	carolKey := invite(t, admin, "carol")
	res = bob.tool("reply", replyPath, map[string]string{"body": "my key is " + carolKey}).expect(t, http.StatusUnprocessableEntity, ErrContentBlocked)
	if res.str("reason") != ReasonBoardKey || !strings.Contains(res.str("hint"), "@carol") {
		t.Fatalf("leak response = %s", res.raw)
	}
	carol := &client{t: t, base: ts.URL}
	carol.tool("join", "/api/join", map[string]string{"key": carolKey}).expect(t, http.StatusForbidden, ErrRevoked)

	// create_thread is gated until the first reply.
	newThread := map[string]string{"board": "webmcp", "title": "Alice asks about tool shapes", "body": "What shape works for you?"}
	alice.tool("create_thread", "/api/threads", newThread).expect(t, http.StatusForbidden, ErrReplyFirst)

	// First reply, then the 20 s cooldown.
	body1 := "Here is a concrete answer with a file name: internal/store/inbox.go."
	res = alice.tool("reply", replyPath, map[string]any{"body": body1, "reply_to": openingID, "stance": "answer"}).expect(t, http.StatusCreated, "")
	post := res.obj("post")
	if post["opening"] != false || post["stance"] != "answer" || post["truncated"] != false || res.str("notice") != Notice {
		t.Fatalf("reply = %s", res.raw)
	}
	if latest := res.list("latest_posts"); len(latest) != 2 {
		t.Fatalf("latest_posts = %v", latest)
	}
	alicePostID := int64(post["id"].(float64))
	res = alice.tool("reply", replyPath, map[string]string{"body": "another one right away"}).expect(t, http.StatusTooManyRequests, ErrRateLimited)
	if res.num("retry_after_s") < 1 || res.header.Get("Retry-After") == "" {
		t.Fatalf("429 body = %s, Retry-After=%q", res.raw, res.header.Get("Retry-After"))
	}
	res = alice.get("/api/whoami").expect(t, http.StatusOK, "")
	if res.body["can_create_thread"] != true || res.str("can_create_thread_reason") != "" || res.obj("quotas")["reply_cooldown_s"].(float64) < 1 {
		t.Fatalf("whoami after reply = %s", res.raw)
	}

	// Duplicate: Bob posts Alice's exact text in the same thread.
	bob.tool("reply", replyPath, map[string]string{"body": body1}).expect(t, http.StatusConflict, ErrDuplicate)

	// Alice may create a thread now; Bob replies in it; Alice's inbox sees it.
	res = alice.tool("create_thread", "/api/threads", newThread).expect(t, http.StatusCreated, "")
	aliceThreadID := int64(res.obj("thread")["id"].(float64))
	if res.obj("post")["opening"] != true || res.obj("thread")["board"] != "webmcp" {
		t.Fatalf("create_thread = %s", res.raw)
	}
	alice.tool("create_thread", "/api/threads", map[string]string{"board": "general", "title": "Second within cooldown", "body": "b"}).expect(t, http.StatusTooManyRequests, ErrRateLimited)
	res = bob.tool("reply", fmt.Sprintf("/api/threads/%d/replies", aliceThreadID), map[string]string{"body": "Bob answers Alice"}).expect(t, http.StatusCreated, "")
	bobPostID := int64(res.obj("post")["id"].(float64))

	res = alice.get("/api/whoami").expect(t, http.StatusOK, "")
	if res.num("inbox_unread") != 1 {
		t.Fatalf("inbox_unread = %s", res.raw)
	}
	alice.tool("get_inbox", "/api/inbox", map[string]any{"limit": 0}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.tool("get_inbox", "/api/inbox", map[string]any{"mark_read": "yes"}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	res = alice.tool("get_inbox", "/api/inbox", map[string]any{"limit": 10, "mark_read": false}).expect(t, http.StatusOK, "")
	items := res.list("items")
	if len(items) != 1 || items[0].(map[string]any)["kind"] != "reply_in_thread" || res.num("cursor") != 0 || res.str("notice") != Notice {
		t.Fatalf("inbox peek = %s", res.raw)
	}
	res = alice.tool("get_inbox", "/api/inbox", `{}`).expect(t, http.StatusOK, "")
	if len(res.list("items")) != 1 || res.num("cursor") != float64(bobPostID) || res.num("unread_remaining") != 0 {
		t.Fatalf("inbox mark_read = %s", res.raw)
	}
	res = alice.tool("get_inbox", "/api/inbox", `{}`).expect(t, http.StatusOK, "")
	if len(res.list("items")) != 0 {
		t.Fatalf("inbox after mark_read = %s", res.raw)
	}

	// Flags: create, duplicate, admin queue, dismiss.
	alice.tool("flag", "/api/flags", map[string]any{"post_id": bobPostID, "reason": "nope"}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.tool("flag", "/api/flags", map[string]any{"reason": "spam"}).expect(t, http.StatusUnprocessableEntity, ErrValidation)
	alice.tool("flag", "/api/flags", map[string]any{"post_id": 99999, "reason": "spam"}).expect(t, http.StatusNotFound, ErrNotFound)
	// A note is agent text: the content rules apply (the flag is where to report a secret, not quote it).
	alice.tool("flag", "/api/flags", map[string]any{"post_id": bobPostID, "reason": "secrets", "note": "leaked ghp_abcdefghijklmnop here"}).expect(t, http.StatusUnprocessableEntity, ErrContentBlocked)
	alice.tool("flag", "/api/flags", map[string]any{"post_id": bobPostID, "reason": "spam", "note": "\u202e"}).expect(t, http.StatusUnprocessableEntity, ErrControlChars)
	res = alice.tool("flag", "/api/flags", map[string]any{"post_id": bobPostID, "reason": "spam", "note": "looks canned"}).expect(t, http.StatusCreated, "")
	flagID := int64(res.num("flag_id"))
	if res.num("post_id") != float64(bobPostID) || res.str("reason") != "spam" {
		t.Fatalf("flag = %s", res.raw)
	}
	alice.tool("flag", "/api/flags", map[string]any{"post_id": bobPostID, "reason": "other"}).expect(t, http.StatusConflict, ErrDuplicate)
	res = admin.admin(http.MethodGet, "/admin/flags", nil).expect(t, http.StatusOK, "")
	flags := res.list("flags")
	if len(flags) != 1 || flags[0].(map[string]any)["reporter"] != "alice" || flags[0].(map[string]any)["note"] != "looks canned" || res.str("notice") != Notice {
		t.Fatalf("admin flags = %s", res.raw)
	}
	admin.admin(http.MethodPost, fmt.Sprintf("/admin/flags/%d/dismiss", flagID), map[string]string{"reason": "fine"}).expect(t, http.StatusOK, "")
	admin.admin(http.MethodPost, fmt.Sprintf("/admin/flags/%d/dismiss", flagID), nil).expect(t, http.StatusNotFound, ErrNotFound)

	// Hide Bob's post: [hidden] in read_thread, 404 in read_post.
	res = admin.admin(http.MethodPost, fmt.Sprintf("/admin/posts/%d/hide", bobPostID), map[string]string{"reason": "test hide"}).expect(t, http.StatusOK, "")
	if res.str("action") != "hide" || res.str("target") != fmt.Sprint(bobPostID) {
		t.Fatalf("hide = %s", res.raw)
	}
	res = alice.get(fmt.Sprintf("/api/threads/%d", aliceThreadID)).expect(t, http.StatusOK, "")
	posts = res.list("posts")
	if len(posts) != 2 || posts[1].(map[string]any)["hidden"] != true || posts[1].(map[string]any)["body"] != store.HiddenBody || res.obj("thread")["reply_count"] != float64(0) {
		t.Fatalf("thread after hide = %s", res.raw)
	}
	alice.get(fmt.Sprintf("/api/posts/%d", bobPostID)).expect(t, http.StatusNotFound, ErrNotFound)
	admin.admin(http.MethodPost, "/admin/posts/99999/hide", nil).expect(t, http.StatusNotFound, ErrNotFound)

	// Lock Alice's thread: replies are refused before any other check.
	admin.admin(http.MethodPost, fmt.Sprintf("/admin/threads/%d/lock", aliceThreadID), nil).expect(t, http.StatusOK, "")
	bob.tool("reply", fmt.Sprintf("/api/threads/%d/replies", aliceThreadID), map[string]string{"body": "too late"}).expect(t, http.StatusLocked, ErrLocked)
	res = alice.get("/api/threads?sort=new").expect(t, http.StatusOK, "")
	if res.list("threads")[0].(map[string]any)["locked"] != true {
		t.Fatalf("locked flag missing: %s", res.raw)
	}

	// Revoke Alice: session gone, key refused, whoami signed out, mod log public.
	admin.admin(http.MethodPost, "/admin/agents/ALICE/revoke", map[string]string{"reason": "test"}).expect(t, http.StatusOK, "")
	admin.admin(http.MethodPost, "/admin/agents/nobody/revoke", nil).expect(t, http.StatusNotFound, ErrNotFound)
	res = alice.get("/api/whoami").expect(t, http.StatusOK, "")
	if res.body["signed_in"] != false {
		t.Fatalf("whoami after revoke = %s", res.raw)
	}
	alice.tool("reply", replyPath, map[string]string{"body": "still here?"}).expect(t, http.StatusUnauthorized, ErrNotSignedIn)
	alice.cookie = ""
	alice.tool("join", "/api/join", map[string]string{"key": aliceKey}).expect(t, http.StatusForbidden, ErrRevoked)
	// The leak event names the poster (@bob) and where the key was headed, not just the victim.
	html := anon.get("/mod-log")
	if html.status != http.StatusOK || !strings.Contains(string(html.raw), "auto_revoke_leak") || !strings.Contains(string(html.raw), "revoke") ||
		!strings.Contains(string(html.raw), "reply body in thread") || !strings.Contains(string(html.raw), "by @bob") {
		t.Fatalf("/mod-log = %d %s", html.status, truncate(string(html.raw), 300))
	}

	// Alice's post is still there for humans, attributed to a revoked agent.
	res = bob.get(fmt.Sprintf("/api/posts/%d", alicePostID)).expect(t, http.StatusOK, "")
	if res.obj("post")["author"].(map[string]any)["handle"] != "alice" {
		t.Fatalf("post author after revoke = %s", res.raw)
	}
}

func TestPagesDocsHealthHeaders(t *testing.T) {
	ts, _ := newTestServer(t, func(c *Config) { c.OTToken = "ot-token" })
	c := &client{t: t, base: ts.URL}

	for _, path := range []string{"/", "/?sort=active&board=webmcp", "/t/1", "/a/board", "/join", "/mod-log", "/stats"} {
		res := c.get(path)
		if res.status != http.StatusOK {
			t.Errorf("GET %s = %d", path, res.status)
			continue
		}
		body := string(res.raw)
		if !strings.Contains(body, `id="webmcp-status"`) {
			t.Errorf("GET %s: badge missing", path)
		}
		if strings.Contains(body, "<script>") || strings.Contains(body, " style=") {
			t.Errorf("GET %s: inline script/style", path)
		}
		h := res.header
		if h.Get("Content-Security-Policy") != contentSecurityPolicy || h.Get("Permissions-Policy") != "tools=(self)" ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "same-origin" || h.Get("Origin-Trial") != "ot-token" {
			t.Errorf("GET %s: security headers = %v", path, h)
		}
		if _, present := h["Origin-Agent-Cluster"]; present {
			t.Errorf("GET %s: Origin-Agent-Cluster must never be sent", path)
		}
	}
	if res := c.get("/"); !strings.Contains(string(res.raw), `http-equiv="refresh" content="30"`) || !strings.Contains(string(res.raw), "Introduce yourself") {
		t.Errorf("index page lacks refresh or seed titles")
	}
	if res := c.get("/t/1"); !strings.Contains(string(res.raw), `id="p1"`) {
		t.Errorf("thread page lacks post anchor")
	}
	for _, path := range []string{"/t/999", "/t/x", "/a/nobody", "/nope"} {
		if res := c.get(path); res.status != http.StatusNotFound || !strings.Contains(res.header.Get("Content-Type"), "text/html") {
			t.Errorf("GET %s = %d %s", path, res.status, res.header.Get("Content-Type"))
		}
	}

	// Docs.
	res := c.get("/skill.md")
	if res.status != 200 || !strings.HasPrefix(res.header.Get("Content-Type"), "text/markdown") || !strings.Contains(string(res.raw), "http://localhost:8080") {
		t.Errorf("/skill.md = %d %s", res.status, res.header.Get("Content-Type"))
	}
	res = c.get("/skill.json")
	if res.status != 200 || res.body == nil || res.body["skill_sha256"] == "" || len(res.list("tools")) != 9 {
		t.Errorf("/skill.json = %d %s", res.status, res.raw)
	}
	for _, path := range []string{"/llms.txt", "/agents.md", "/static/board.js", "/static/board.css"} {
		if res := c.get(path); res.status != 200 {
			t.Errorf("GET %s = %d", path, res.status)
		}
	}
	// The manifest's sha256 is the sha256 of the served skill.md (stable across requests), and
	// agents.md is byte-identical to it.
	skill := c.get("/skill.md")
	if got := sha256Hex(skill.raw); got != res.str("skill_sha256") {
		t.Errorf("skill.json skill_sha256 = %s, sha256(/skill.md) = %s", res.str("skill_sha256"), got)
	}
	if again := c.get("/skill.md"); !bytes.Equal(again.raw, skill.raw) {
		t.Errorf("/skill.md differs between two requests")
	}
	if alias := c.get("/agents.md"); !bytes.Equal(alias.raw, skill.raw) {
		t.Errorf("/agents.md differs from /skill.md")
	}
	if strings.Contains(string(skill.raw), "generated ") {
		t.Errorf("/skill.md embeds a render timestamp")
	}
	if cc := res.header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("/skill.json Cache-Control = %q, want no-cache", cc)
	}
	// No directory listing of the embedded static tree.
	if res := c.get("/static/"); res.status != http.StatusNotFound {
		t.Errorf("GET /static/ = %d, want 404", res.status)
	}

	// Health: booleans only, never the snapshot error text.
	res = c.get("/health")
	if res.status != 200 || res.body["ok"] != true || res.body["db"] != true || res.body["snapshot_enabled"] != false || res.str("version") != "test" || res.body["snapshot_failing"] != false {
		t.Errorf("/health = %d %s", res.status, res.raw)
	}
	if _, present := res.body["snapshot_age_s"]; !present {
		t.Errorf("/health lacks snapshot_age_s: %s", res.raw)
	}
	if _, present := res.body["last_snapshot_error"]; present {
		t.Errorf("/health exposes last_snapshot_error: %s", res.raw)
	}

	// https redirect, with the security headers.
	res = c.do(http.MethodGet, "/t/1?page=2", map[string]string{"X-Forwarded-Proto": "http"}, nil)
	if res.status != http.StatusMovedPermanently || !strings.HasPrefix(res.header.Get("Location"), "https://") || !strings.HasSuffix(res.header.Get("Location"), "/t/1?page=2") {
		t.Errorf("https redirect = %d %s", res.status, res.header.Get("Location"))
	}
	if res.header.Get("Content-Security-Policy") != contentSecurityPolicy {
		t.Errorf("301 lacks CSP: %v", res.header)
	}

	// /admin is 404 when no token is configured.
	ts2, _ := newTestServer(t, func(c *Config) { c.AdminToken = "" })
	c2 := &client{t: t, base: ts2.URL}
	c2.admin(http.MethodGet, "/admin/flags", nil).expect(t, http.StatusNotFound, ErrNotFound)
}

func TestJoinForm(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	admin := &client{t: t, base: ts.URL}
	key := invite(t, admin, "formie")
	c := &client{t: t, base: ts.URL}
	form := func(key string) result {
		return c.do(http.MethodPost, "/join", map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			url.Values{"key": {key}}.Encode())
	}
	if res := form("garbage"); res.status != http.StatusUnprocessableEntity || !strings.Contains(string(res.raw), `role="alert"`) || !strings.Contains(string(res.raw), "43 base64url") {
		t.Errorf("form bad key = %d %s", res.status, truncate(string(res.raw), 200))
	}
	unknown, _, _ := store.GenerateKey()
	if res := form(unknown); res.status != http.StatusUnauthorized {
		t.Errorf("form unknown key = %d", res.status)
	}
	res := form(key)
	if res.status != http.StatusSeeOther || res.header.Get("Location") != "/join?joined=formie" {
		t.Fatalf("form join = %d %s", res.status, res.header.Get("Location"))
	}
	sc := res.header.Get("Set-Cookie")
	if !strings.Contains(sc, CookieName+"=") || !strings.Contains(sc, "Max-Age=7776000") || !strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "Secure") {
		t.Fatalf("form Set-Cookie = %s", sc)
	}
	if res := c.get("/join?joined=formie"); !strings.Contains(string(res.raw), `role="status"`) || !strings.Contains(string(res.raw), "formie") {
		t.Errorf("joined confirmation missing: %s", truncate(string(res.raw), 300))
	}
	if res := c.get("/join?joined=<b>nope</b>"); strings.Contains(string(res.raw), `role="status"`) || strings.Contains(string(res.raw), "nope") {
		t.Errorf("unknown ?joined echoed")
	}
}

func TestEdgeKey(t *testing.T) {
	ts, _ := newTestServer(t, func(c *Config) { c.EdgeKey = "edge-secret" })
	c := &client{t: t, base: ts.URL}
	if res := c.get("/"); res.status != http.StatusMisdirectedRequest || !strings.Contains(res.header.Get("Content-Type"), "text/html") ||
		res.header.Get("Content-Security-Policy") != contentSecurityPolicy || res.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("GET / without edge key = %d %s %v", res.status, res.header.Get("Content-Type"), res.header)
	}
	c.get("/api/whoami").expect(t, http.StatusMisdirectedRequest, ErrEdge)
	if res := c.do(http.MethodGet, "/api/whoami", map[string]string{HeaderEdgeKey: "wrong"}, nil); res.status != http.StatusMisdirectedRequest {
		t.Errorf("wrong edge key = %d", res.status)
	}
	if res := c.do(http.MethodGet, "/api/whoami", map[string]string{HeaderEdgeKey: "edge-secret"}, nil); res.status != http.StatusOK {
		t.Errorf("right edge key = %d", res.status)
	}
	if res := c.get("/health"); res.status != http.StatusOK {
		t.Errorf("/health must bypass the edge key: %d", res.status)
	}
}

func TestIPLimits(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	c := &client{t: t, base: ts.URL, extra: http.Header{"X-Test-Ip": {"203.0.113.9"}}}

	// join: 10 attempts per hour per IP; failures count.
	unknown, _, _ := store.GenerateKey()
	for i := 0; i < JoinPerHourPerIP; i++ {
		c.tool("join", "/api/join", map[string]string{"key": unknown}).expect(t, http.StatusUnauthorized, ErrInvalidKey)
	}
	res := c.tool("join", "/api/join", map[string]string{"key": unknown}).expect(t, http.StatusTooManyRequests, ErrRateLimited)
	if res.num("retry_after_s") < 1 || res.header.Get("Retry-After") == "" {
		t.Fatalf("join 429 = %s", res.raw)
	}
	// Another address is unaffected.
	other := &client{t: t, base: ts.URL, extra: http.Header{"X-Test-Ip": {"203.0.113.10"}}}
	other.tool("join", "/api/join", map[string]string{"key": unknown}).expect(t, http.StatusUnauthorized, ErrInvalidKey)

	// reads: 120 per minute per IP when signed out.
	for i := 0; i < ReadsPerMinute; i++ {
		other.get("/api/whoami").expect(t, http.StatusOK, "")
	}
	other.get("/api/whoami").expect(t, http.StatusTooManyRequests, ErrRateLimited)
	// Header absent: the limiter falls back to RemoteAddr instead of failing open.
	bare := &client{t: t, base: ts.URL}
	bare.get("/api/whoami").expect(t, http.StatusOK, "")
	for i := 0; i < JoinPerHourPerIP; i++ {
		bare.tool("join", "/api/join", map[string]string{"key": unknown}).expect(t, http.StatusUnauthorized, ErrInvalidKey)
	}
	bare.tool("join", "/api/join", map[string]string{"key": unknown}).expect(t, http.StatusTooManyRequests, ErrRateLimited)
}

// TestConcurrentWrites fires parallel writes from one agent and asserts the per-agent limits
// hold: exactly one reply lands in a burst (1 per 20 s), and one body posted at the same
// moment by several agents lands once (duplicate check inside the write transaction).
func TestConcurrentWrites(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	admin := &client{t: t, base: ts.URL}
	alice := &client{t: t, base: ts.URL}
	join(t, alice, invite(t, admin, "alice"))
	res := alice.get("/api/threads").expect(t, http.StatusOK, "")
	threadID := int64(res.list("threads")[0].(map[string]any)["id"].(float64))
	replyPath := fmt.Sprintf("/api/threads/%d/replies", threadID)

	const n = 12
	statuses := make(chan int, n)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			statuses <- alice.tool("reply", replyPath, map[string]string{"body": fmt.Sprintf("burst reply %d", i)}).status
		}(i)
	}
	start.Done()
	done.Wait()
	close(statuses)
	counts := map[int]int{}
	for st := range statuses {
		counts[st]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusTooManyRequests] != n-1 {
		t.Fatalf("burst of %d replies -> %v, want exactly one 201 and %d 429", n, counts, n-1)
	}
	res = alice.get(fmt.Sprintf("/api/threads/%d", threadID)).expect(t, http.StatusOK, "")
	if res.obj("thread")["reply_count"] != float64(1) {
		t.Fatalf("reply_count = %v, want 1", res.obj("thread")["reply_count"])
	}

	// Same body from four agents at once: one 201, three 409.
	agents := make([]*client, 4)
	for i := range agents {
		agents[i] = &client{t: t, base: ts.URL}
		join(t, agents[i], invite(t, admin, fmt.Sprintf("dup%d", i)))
	}
	statuses = make(chan int, len(agents))
	start = sync.WaitGroup{}
	start.Add(1)
	for _, a := range agents {
		done.Add(1)
		go func(a *client) {
			defer done.Done()
			start.Wait()
			statuses <- a.tool("reply", replyPath, map[string]string{"body": "the very same sentence"}).status
		}(a)
	}
	start.Done()
	done.Wait()
	close(statuses)
	counts = map[int]int{}
	for st := range statuses {
		counts[st]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusConflict] != len(agents)-1 {
		t.Fatalf("same body from %d agents -> %v, want one 201 and %d 409", len(agents), counts, len(agents)-1)
	}
}

func TestNormalizeAndBlocked(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"hello", "hello", true},
		{"  a \r\nb  \n\n\n\nc\t \n", "a\nb\n\nc", true},
		{"zero\u200bwidth\ufeff", "zerowidth", true},
		{"ctrl\x01\x7fchars", "ctrlchars", true},
		{"é", "é", true}, // NFC composes
		{"bidi \u202e", "", false},
		{"iso \u2066x\u2069", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeText(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("normalizeText(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if singleLine("a\nb\t\tc") != "a b c" {
		t.Errorf("singleLine")
	}
	blocked := map[string]string{
		"key ab_" + strings.Repeat("A", 43):                  "board_key",
		"0x52908400098527886E0F7030069857D2E4169EE7":         "eth_address",
		"pay bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq now": "btc_address",
		"presale of $ABC":                                               "crypto_promo",
		"sk-ant-api03-abcdefghijkl":                                     "secret",
		"AKIAIOSFODNN7EXAMPLE":                                          "secret",
		"-----BEGIN RSA PRIVATE KEY-----":                               "secret",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig":                      "jwt",
		"plain prose mentioning sk- and ghp_ and an airdrop of nothing": "",
		"a $BIG deal":                                                   "",
	}
	for in, want := range blocked {
		if got := checkBlocked(in); got != want {
			t.Errorf("checkBlocked(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", ClipBodyChars+1)
	if body, trunc := clipBody(long); !trunc || len(body) != ClipBodyChars {
		t.Errorf("clipBody")
	}
}

func TestCanonicalHost(t *testing.T) {
	ts, _ := newTestServer(t, func(c *Config) { c.BaseURL = "https://board.example.test" })
	c := &client{t: t, base: ts.URL}

	// The edge appends the hostname the client used after any client-supplied value.
	res := c.do(http.MethodGet, "/t/1?page=2", map[string]string{"X-Forwarded-Host": "spoof.example, alias.datumproxy.net"}, nil)
	if res.status != http.StatusMovedPermanently || res.header.Get("Location") != "https://board.example.test/t/1?page=2" {
		t.Errorf("alias host: got %d %q, want 301 to the canonical origin", res.status, res.header.Get("Location"))
	}
	if res.header.Get("Content-Security-Policy") != contentSecurityPolicy {
		t.Errorf("301 lacks CSP: %v", res.header)
	}

	// The canonical host anywhere in the list is served, whatever else a hop appended.
	for _, xfh := range []string{"board.example.test", "Board.Example.Test", "spoof.example, board.example.test", "board.example.test, origin.internal"} {
		res = c.do(http.MethodGet, "/", map[string]string{"X-Forwarded-Host": xfh}, nil)
		if res.status != http.StatusOK {
			t.Errorf("X-Forwarded-Host %q: got %d, want 200", xfh, res.status)
		}
	}

	// No header (local dev, direct origin) and /health are left alone.
	if res = c.do(http.MethodGet, "/", nil, nil); res.status != http.StatusOK {
		t.Errorf("no header: got %d, want 200", res.status)
	}
	if res = c.do(http.MethodGet, "/health", map[string]string{"X-Forwarded-Host": "alias.datumproxy.net"}, nil); res.status != http.StatusOK {
		t.Errorf("/health via alias: got %d, want 200", res.status)
	}
}
