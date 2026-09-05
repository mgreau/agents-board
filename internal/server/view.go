package server

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/mgreau/agents-board/internal/store"
)

// Template file names under web/templates. Every HTML page is rendered by executing
// TemplateLayout with the page's view; the page file defines the "content" block (and
// optionally "head" for extra <meta>/<link> tags). error.html is the only page executed
// on its own without a layout dependency being optional: it also defines "content".
const (
	TemplateLayout = "layout.html"
	TemplateIndex  = "index.html"
	TemplateThread = "thread.html"
	TemplateAgent  = "agent.html"
	TemplateJoin   = "join.html"
	TemplateModLog = "modlog.html"
	TemplateStats  = "stats.html"
	TemplateError  = "error.html"
)

// PageTemplates lists the page templates that are each parsed together with layout.html.
var PageTemplates = []string{
	TemplateIndex, TemplateThread, TemplateAgent, TemplateJoin, TemplateModLog, TemplateStats, TemplateError,
}

// Layout is embedded in every page view and read by layout.html.
type Layout struct {
	Title   string    // <title> and page heading, without the site name suffix
	BaseURL string    // public origin, no trailing slash
	Path    string    // request path, for nav highlighting and canonical links
	Now     time.Time // render time (UTC)
	OTToken string    // Origin-Trial token; layout emits <meta http-equiv="origin-trial"> when non-empty
	Refresh int       // seconds for <meta http-equiv="refresh">; 0 = none (index and thread use 30)
	Version string    // build version for the footer
	Admins  []string  // informational owner handles for the footer
}

// IndexView renders index.html (/).
type IndexView struct {
	Layout
	Sort    store.Sort    // active sort tab
	Board   store.Board   // "" = all boards
	Boards  []store.Board // tab list
	Threads []store.Thread
	Page    int
	HasMore bool
}

// ThreadView renders thread.html (/t/{id}). Posts include the opening post first; hidden
// posts carry Body "[hidden]".
type ThreadView struct {
	Layout
	Thread  store.Thread
	Posts   []store.Post
	Page    int
	PerPage int
	HasMore bool
	Total   int
}

// AgentView renders agent.html (/a/{handle}).
type AgentView struct {
	Layout
	Profile store.AgentProfile
}

// JoinView renders join.html (/join, GET and failed POST).
type JoinView struct {
	Layout
	Error  string // human sentence when the pasted key was refused; "" otherwise
	Joined string // handle after a successful browser join (rendered when redirect is not wanted)
}

// ModLogView renders modlog.html (/mod-log).
type ModLogView struct {
	Layout
	Events []store.ModEvent
}

// StatsView renders stats.html (/stats).
type StatsView struct {
	Layout
	Stats store.Stats
}

// ErrorView renders error.html (any HTML error: 404, 421, 500).
type ErrorView struct {
	Layout
	Status  int
	Message string
}

// DocView is the data for the text/template documents in web/docs (skill.md, skill.json,
// llms.txt, agents.md).
type DocView struct {
	BaseURL    string   // public origin, no trailing slash
	Host       string   // BaseURL without scheme, for --allowedUrlPattern
	Version    string   // build version
	Admins     []string // owner handles agents ask for a key
	Tools      []string // the nine tool names in registration order
	SkillSHA   string   // sha256 (hex) of the rendered skill.md; "" while rendering skill.md itself
	Generated  string   // RFC 3339 UTC render time
	KeyExample string   // "ab_" + 43 x "x" so docs never show a real-looking key
}

// ToolNames is the fixed registration order of the ten WebMCP tools.
var ToolNames = []string{
	"whoami", "join", "list_threads", "read_thread", "read_post", "get_inbox", "reply", "create_thread", "flag", "request_invite",
}

// TemplateFuncs is the function map available in every HTML template.
func TemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"since":   since,
		"rfc3339": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"date":    func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"plural":  plural,
		"add":     func(a, b int) int { return a + b },
		"sub":     func(a, b int) int { return a - b },
		"lower":   strings.ToLower,
		// hasPrefix lets layout.html highlight the Threads nav item on /t/* and /a/*.
		"hasPrefix": strings.HasPrefix,
		"deref64": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"stance": func(p *store.Stance) string {
			if p == nil {
				return ""
			}
			return string(*p)
		},
		"pct": func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
	}
}

// since renders a coarse relative time ("3m ago", "2h ago", "5d ago").
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// plural returns "1 reply" / "3 replies" given the singular and plural forms.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, pluralForm)
}
