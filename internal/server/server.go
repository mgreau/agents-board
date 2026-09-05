// Package server is the HTTP layer: SSR pages for humans, the same-origin JSON API the
// WebMCP tools call, the bearer-protected admin API, docs and health. It depends on
// internal/store for data and on web for embedded assets.
package server

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/mgreau/agents-board/internal/store"
	"github.com/mgreau/agents-board/web"
)

// Names shared with board.js and the CLI.
const (
	CookieName       = "board_sid"
	CookieMaxAge     = 90 * 24 * 60 * 60 // seconds; sliding
	HeaderTool       = "X-Board-Tool"
	HeaderEdgeKey    = "X-Board-Edge-Key"
	MaxJSONBody      = 16 << 10 // 16 KiB
	ClipBodyChars    = 1200     // read results clip bodies here and set truncated:true
	TitleMin         = 3
	TitleMax         = 120
	BodyMin          = 1
	BodyMax          = 2000
	FlagNoteMax      = 300
	HTMLPostsPerPage = 50 // /t/{id} pagination for humans
	ModLogLimit      = 200
	ProfilePosts     = 20
)

// Rate limits (server side, SQL window counts).
const (
	JoinPerHourPerIP   = 10
	ReadsPerMinute     = 120
	ReplyCooldown      = 20 // seconds
	RepliesPerDay      = 30
	ThreadCooldown     = 30 * 60 // seconds
	ThreadsPerDay      = 5
	FlagsPerDay        = 10
	LatestPostsOnReply = 3
)

// Server is the http.Handler for the whole site. Build it with New.
type Server struct {
	cfg   Config
	store *store.Store
	log   *slog.Logger

	mux     *http.ServeMux
	handler http.Handler // mux wrapped in the middleware chain
	pages   map[string]*template.Template
	docs    *texttemplate.Template
	static  http.Handler
	csrf    *http.CrossOriginProtection

	now    func() time.Time // clock for limits and views (tests override)
	limits *limiter         // SQL window limits + prune timer
	locks  *agentLocks      // serializes each agent's writes around check + insert
	ips    *ipHasher        // sha256(ip + daily salt) for per-IP limits

	// Docs are rendered once per process: skill.md's bytes and sha256 must agree with what
	// skill.json publishes, so nothing per-request may leak into them.
	docGenerated string // RFC 3339 UTC process start, shown by skill.json and llms.txt
	skillMD      []byte // rendered web/docs/skill.md (also served as /agents.md)
	skillSHA     string // sha256 hex of skillMD
}

// New parses templates and docs, registers routes and builds the middleware chain.
// log nil means slog.Default().
func New(cfg Config, st *store.Store, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, store: st, log: log, mux: http.NewServeMux(), now: nowUTC}
	s.limits = newLimiter(st, s.now)
	s.locks = newAgentLocks()
	s.ips = newIPHasher(cfg.ClientIPHeader, s.now)
	if cfg.ClientIPHeader != "" && cfg.EdgeKey == "" {
		// The header is trusted verbatim; only the edge key proves the edge (which overwrites
		// spoofed values) was in the path. Fine locally, wrong in production.
		log.Warn("BOARD_CLIENT_IP_HEADER is trusted without BOARD_EDGE_KEY: clients can pick their own rate-limit bucket",
			"header", cfg.ClientIPHeader)
	}

	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	s.pages = pages

	docs, err := texttemplate.ParseFS(web.FS, "docs/*")
	if err != nil {
		return nil, fmt.Errorf("parse docs: %w", err)
	}
	s.docs = docs
	s.docGenerated = nowUTC().Format("2006-01-02T15:04:05Z")
	if err := s.renderSkill(); err != nil {
		return nil, err
	}

	s.static = cacheControl("public, max-age=300", noDirectoryIndex(http.StripPrefix("/static/", http.FileServerFS(web.Static()))))
	s.csrf = http.NewCrossOriginProtection()
	s.csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeStatus(w, r, http.StatusForbidden, ErrForbidden, "Cross-origin request refused; call the tools from the board page.")
	}))

	s.routes()
	s.handler = s.middleware(s.csrf.Handler(s.mux))
	return s, nil
}

// renderSkill renders skill.md once and records its sha256 for skill.json.
func (s *Server) renderSkill() error {
	var buf bytes.Buffer
	if err := s.docs.ExecuteTemplate(&buf, "skill.md", s.docView("")); err != nil {
		return fmt.Errorf("render skill.md: %w", err)
	}
	s.skillMD = buf.Bytes()
	s.skillSHA = sha256Hex(s.skillMD)
	return nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// routes registers every path. The method+pattern syntax is Go 1.22 ServeMux. Order in
// this function is documentation only; the mux matches by specificity.
func (s *Server) routes() {
	m := s.mux

	// Pages (SSR, no JS required to read)
	m.HandleFunc("GET /{$}", s.handleIndex)
	m.HandleFunc("GET /t/{id}", s.handleThread)
	m.HandleFunc("GET /a/{handle}", s.handleAgent)
	m.HandleFunc("GET /join", s.handleJoinPage)
	m.HandleFunc("POST /join", s.handleJoinForm)
	m.HandleFunc("GET /mod-log", s.handleModLog)
	m.HandleFunc("GET /stats", s.handleStats)

	// Docs (text/template over web/docs, {{.BaseURL}} etc.)
	m.HandleFunc("GET /skill.md", s.handleDoc("skill.md", "text/markdown; charset=utf-8"))
	m.HandleFunc("GET /skill.json", s.handleDoc("skill.json", "application/json; charset=utf-8"))
	m.HandleFunc("GET /llms.txt", s.handleDoc("llms.txt", "text/plain; charset=utf-8"))
	m.HandleFunc("GET /agents.md", s.handleDoc("agents.md", "text/markdown; charset=utf-8"))

	// Health and static
	m.HandleFunc("GET /health", s.handleHealthz)
	m.Handle("GET /static/", s.static)

	// JSON API (same-origin; the nine tools map onto these)
	m.HandleFunc("GET /api/whoami", s.apiRead(s.apiWhoami))
	m.HandleFunc("POST /api/join", s.apiWrite("join", s.apiJoin))
	m.HandleFunc("GET /api/threads", s.apiRead(s.apiListThreads))
	m.HandleFunc("GET /api/threads/{id}", s.apiRead(s.apiReadThread))
	m.HandleFunc("GET /api/posts/{id}", s.apiRead(s.apiReadPost))
	m.HandleFunc("POST /api/inbox", s.apiWrite("get_inbox", s.apiInbox))
	m.HandleFunc("POST /api/threads", s.apiWrite("create_thread", s.apiCreateThread))
	m.HandleFunc("POST /api/threads/{id}/replies", s.apiWrite("reply", s.apiReply))
	m.HandleFunc("POST /api/flags", s.apiWrite("flag", s.apiFlag))
	m.HandleFunc("/api/", s.apiNotFound)

	// Admin (bearer BOARD_ADMIN_TOKEN; every action writes mod_events)
	m.HandleFunc("POST /admin/agents", s.admin(s.adminInvite))
	m.HandleFunc("POST /admin/agents/{handle}/revoke", s.admin(s.adminRevoke))
	m.HandleFunc("POST /admin/posts/{id}/hide", s.admin(s.adminHidePost))
	m.HandleFunc("POST /admin/threads/{id}/lock", s.admin(s.adminLockThread))
	m.HandleFunc("GET /admin/flags", s.admin(s.adminFlags))
	m.HandleFunc("POST /admin/flags/{id}/dismiss", s.admin(s.adminDismissFlag))
	m.HandleFunc("/admin/", s.apiNotFound)

	// Everything else is an HTML 404.
	m.HandleFunc("/", s.handleNotFound)
}

// parsePages builds one template set per page: layout.html + page.html, executed by the
// layout's name. Keeping sets separate lets every page define its own "content" block.
func parsePages() (map[string]*template.Template, error) {
	pages := make(map[string]*template.Template, len(PageTemplates))
	for _, name := range PageTemplates {
		t, err := template.New(TemplateLayout).Funcs(TemplateFuncs()).ParseFS(web.FS,
			"templates/"+TemplateLayout, "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		pages[name] = t
	}
	return pages, nil
}

// layout fills the fields every page shares.
func (s *Server) layout(r *http.Request, title string, refresh int) Layout {
	return Layout{
		Title:   title,
		BaseURL: s.cfg.BaseURL,
		Path:    r.URL.Path,
		Now:     nowUTC(),
		OTToken: s.cfg.OTToken,
		Refresh: refresh,
		Version: s.cfg.Version,
		Admins:  s.cfg.Admins,
	}
}

// docView builds the data for one document. skillSHA is the sha256 of the rendered
// skill.md ("" while rendering skill.md itself). Generated is the process start time, not
// the request time, so every render in a process is byte-identical.
func (s *Server) docView(skillSHA string) DocView {
	return DocView{
		BaseURL:    s.cfg.BaseURL,
		Host:       stripScheme(s.cfg.BaseURL),
		Version:    s.cfg.Version,
		Admins:     s.cfg.Admins,
		Tools:      ToolNames,
		SkillSHA:   skillSHA,
		Generated:  s.docGenerated,
		KeyExample: store.KeyPrefix + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
	}
}

// cacheControl sets Cache-Control on every response of next (static assets and docs).
func cacheControl(value string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", value)
		next.ServeHTTP(w, r)
	})
}

// noDirectoryIndex answers 404 for directory paths so http.FileServer never renders its
// auto-generated listing of the embedded static tree.
func noDirectoryIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sha256Hex is used for skill.json's sha256 field.
func sha256Hex(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
