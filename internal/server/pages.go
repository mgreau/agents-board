package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/mgreau/agents-board/internal/store"
)

// Page handlers (SSR). Each builds a view from view.go and calls render.

// pageRefresh is the <meta http-equiv="refresh"> interval for index and thread pages.
const pageRefresh = 30

// handleIndex serves / : threads with sort tabs (?sort=unanswered|active|new, default
// unanswered) and board filter (?board=general|introductions|webmcp), 20 per ?page.
// Unknown sort or board values fall back to the defaults: humans get a page, not a 422.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sort := store.Sort(q.Get("sort"))
	if !store.ValidSort(string(sort)) {
		sort = store.SortUnanswered
	}
	board := store.Board(q.Get("board"))
	if !store.ValidBoard(string(board)) {
		board = ""
	}
	page, ok := queryInt(r, "page", 1)
	if !ok {
		page = 1
	}
	res, err := s.store.ListThreads(r.Context(), store.ThreadListParams{Board: board, Sort: sort, Page: page, PerPage: store.ThreadsPerPage})
	if err != nil {
		s.pageError(w, r, "list threads", err)
		return
	}
	s.render(w, r, http.StatusOK, TemplateIndex, IndexView{
		Layout: s.layout(r, "Threads", pageRefresh), Sort: sort, Board: board, Boards: store.Boards,
		Threads: res.Threads, Page: res.Page, HasMore: res.HasMore,
	})
}

// handleThread serves /t/{id}: thread header + posts oldest first, HTMLPostsPerPage per
// ?page, hidden posts as "[hidden]". 404 for unknown or hidden threads. Layout.Refresh = 30.
func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "No such thread.")
		return
	}
	page, ok := queryInt(r, "page", 1)
	if !ok {
		page = 1
	}
	t, err := s.store.ThreadByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "No such thread.")
		return
	}
	if err != nil {
		s.pageError(w, r, "thread by id", err)
		return
	}
	posts, err := s.store.ThreadPosts(r.Context(), id, page, HTMLPostsPerPage)
	if err != nil {
		s.pageError(w, r, "thread posts", err)
		return
	}
	s.render(w, r, http.StatusOK, TemplateThread, ThreadView{
		Layout: s.layout(r, t.Title, pageRefresh), Thread: *t, Posts: posts.Posts,
		Page: posts.Page, PerPage: posts.PerPage, HasMore: posts.HasMore, Total: posts.Total,
	})
}

// handleAgent serves /a/{handle}: provenance, counts, ProfilePosts most recent posts.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	prof, err := s.store.AgentProfile(r.Context(), r.PathValue("handle"), ProfilePosts)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "No such agent.")
		return
	}
	if err != nil {
		s.pageError(w, r, "agent profile", err)
		return
	}
	s.render(w, r, http.StatusOK, TemplateAgent, AgentView{Layout: s.layout(r, "@"+prof.Agent.Handle, 0), Profile: *prof})
}

// handleJoinPage serves GET /join: how to join + a form posting `key` to POST /join.
// ?joined=<handle> (set by the redirect after a successful form post) shows the
// confirmation; it is only echoed when it names a real agent.
func (s *Server) handleJoinPage(w http.ResponseWriter, r *http.Request) {
	view := JoinView{Layout: s.layout(r, "Join", 0)}
	if h := r.URL.Query().Get("joined"); h != "" {
		if a, err := s.store.AgentByHandle(r.Context(), h); err == nil {
			view.Joined = a.Handle
		}
	}
	s.render(w, r, http.StatusOK, TemplateJoin, view)
}

// handleJoinForm serves POST /join (application/x-www-form-urlencoded, field `key`): same
// checks and rate limit as /api/join; on success sets the board_sid cookie and 303s to
// /join?joined=<handle>; on failure re-renders join.html with JoinView.Error and the
// matching status. CrossOriginProtection already rejected cross-site posts.
func (s *Server) handleJoinForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBody)
	if err := r.ParseForm(); err != nil {
		s.render(w, r, http.StatusBadRequest, TemplateJoin, JoinView{Layout: s.layout(r, "Join", 0), Error: "Could not read the form."})
		return
	}
	out := s.join(r, r.PostForm.Get("key"))
	if out.agent == nil {
		if out.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", strconv.Itoa(out.retry))
		}
		s.render(w, r, out.status, TemplateJoin, JoinView{Layout: s.layout(r, "Join", 0), Error: out.hint})
		return
	}
	s.setSessionCookie(w, out.token)
	http.Redirect(w, r, "/join?joined="+url.QueryEscape(out.agent.Handle), http.StatusSeeOther)
}

// handleModLog serves /mod-log: newest ModLogLimit mod_events.
func (s *Server) handleModLog(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ModEvents(r.Context(), ModLogLimit)
	if err != nil {
		s.pageError(w, r, "mod events", err)
		return
	}
	s.render(w, r, http.StatusOK, TemplateModLog, ModLogView{Layout: s.layout(r, "Moderation log", 0), Events: events})
}

// handleStats serves /stats.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		s.pageError(w, r, "stats", err)
		return
	}
	s.render(w, r, http.StatusOK, TemplateStats, StatsView{Layout: s.layout(r, "Stats", 0), Stats: *st})
}

// handleDoc returns a handler serving web/docs/<name> as contentType. skill.md (and its
// alias agents.md) are served from the bytes rendered at boot so that skill.json's
// skill_sha256, computed from the same bytes, always matches what agents download. The
// manifest itself is not cached by clients (no-cache) so it can never lag behind skill.md.
func (s *Server) handleDoc(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := s.skillMD
		cache := "public, max-age=300"
		if name != "skill.md" && name != "agents.md" {
			var buf bytes.Buffer
			if err := s.docs.ExecuteTemplate(&buf, name, s.docView(s.skillSHA)); err != nil {
				s.log.ErrorContext(r.Context(), "render doc", "name", name, "err", err)
				s.renderError(w, r, http.StatusInternalServerError, "Could not render documentation.")
				return
			}
			body = buf.Bytes()
		}
		if name == "skill.json" {
			cache = "no-cache"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", cache)
		_, _ = w.Write(body)
	}
}

// healthJSON is the /health body: store.Health plus the build version.
type healthJSON struct {
	store.Health
	Version string `json:"version"`
}

// handleHealthz serves GET /health: {"ok":bool,"db":bool,"snapshot_enabled":bool,
// "snapshot_age_s":number|null,"snapshot_dirty":bool,"snapshot_failing":bool,
// "version":string}. 200 when ok, 503 otherwise. Exempt from the edge key and public, so
// the snapshot error text stays in the logs.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	h := s.store.Health(r.Context())
	status := http.StatusOK
	if !h.OK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, healthJSON{Health: h, Version: s.cfg.Version})
}

// handleNotFound renders the HTML 404.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.renderError(w, r, http.StatusNotFound, "There is nothing at this address.")
}

// pageError logs err and renders the HTML 500.
func (s *Server) pageError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.ErrorContext(r.Context(), what, "err", err, "path", r.URL.Path)
	s.renderError(w, r, http.StatusInternalServerError, "Something broke on our side. Retry in a minute.")
}

// render executes page template `name` with view, buffering so a template error becomes
// a clean 500 instead of a half-written page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, view any) {
	t, ok := s.pages[name]
	if !ok {
		s.log.ErrorContext(r.Context(), "unknown template", "name", name)
		s.renderError(w, r, http.StatusInternalServerError, "Template missing.")
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, TemplateLayout, view); err != nil {
		s.log.ErrorContext(r.Context(), "render", "template", name, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Could not render this page.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderError renders error.html. It never recurses: if error.html itself fails it falls
// back to plain text.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	view := ErrorView{Layout: s.layout(r, fmt.Sprintf("%d %s", status, http.StatusText(status)), 0), Status: status, Message: message}
	t, ok := s.pages[TemplateError]
	if ok {
		var buf bytes.Buffer
		if err := t.ExecuteTemplate(&buf, TemplateLayout, view); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			_, _ = buf.WriteTo(w)
			return
		}
	}
	http.Error(w, fmt.Sprintf("%d %s: %s", status, http.StatusText(status), message), status)
}
