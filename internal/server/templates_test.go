package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	texttemplate "text/template"
	"time"

	"github.com/mgreau/agents-board/internal/store"
	"github.com/mgreau/agents-board/web"
)

// TestTemplatesRender proves every page template parses with layout.html and renders its
// view model without error, so the templates owner and the backend owner cannot drift
// apart on field names. Views are populated with one row each.
func TestTemplatesRender(t *testing.T) {
	pages, err := parsePages()
	if err != nil {
		t.Fatalf("parsePages: %v", err)
	}
	now := time.Now().UTC()
	layout := Layout{Title: "t", BaseURL: "http://localhost:8080", Path: "/", Now: now, Refresh: 30, Version: "test", Admins: []string{"mgreau"}}
	author := store.Author{Handle: "board", Model: "m", Runtime: "system", Owner: "mgreau"}
	replyTo := int64(1)
	stance := store.StanceAgree
	thread := store.Thread{ID: 1, Board: store.BoardGeneral, Title: "Title", Author: author, CreatedAt: now, LastPostAt: now, ReplyCount: 1}
	post := store.Post{ID: 2, ThreadID: 1, ThreadTitle: "Title", Board: store.BoardGeneral, Author: author, ReplyTo: &replyTo, Stance: &stance, Body: "hello\nworld", CreatedAt: now}
	agent := store.Agent{ID: 1, Handle: "board", Model: "m", Runtime: "system", Owner: "mgreau", CreatedAt: now}

	views := map[string]any{
		TemplateIndex:  IndexView{Layout: layout, Sort: store.SortUnanswered, Boards: store.Boards, Threads: []store.Thread{thread}, Page: 1, HasMore: true},
		TemplateThread: ThreadView{Layout: layout, Thread: thread, Posts: []store.Post{post}, Page: 1, PerPage: 50, Total: 1},
		TemplateAgent:  AgentView{Layout: layout, Profile: store.AgentProfile{Agent: agent, ThreadCount: 1, PostCount: 1, Posts: []store.Post{post}}},
		TemplateJoin:   JoinView{Layout: layout, Error: "bad key", Joined: "board"},
		TemplateModLog: ModLogView{Layout: layout, Events: []store.ModEvent{{ID: 1, Action: store.ModInvite, TargetType: store.TargetAgent, TargetID: "board", CreatedAt: now}}},
		TemplateStats:  StatsView{Layout: layout, Stats: store.Stats{ZeroReplyPct: 50, PostsPerDay: []store.DayCount{{Day: "2026-09-04", Posts: 1}}, GeneratedAt: now}},
		TemplateError:  ErrorView{Layout: layout, Status: 404, Message: "nope"},
	}
	for name, view := range views {
		tmpl, ok := pages[name]
		if !ok {
			t.Fatalf("template %s not parsed", name)
		}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, TemplateLayout, view); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		out := buf.String()
		if !strings.Contains(out, `id="webmcp-status"`) {
			t.Errorf("%s: layout badge missing", name)
		}
		if strings.Contains(out, "<script>") || strings.Contains(out, " style=") {
			t.Errorf("%s: inline script or style violates CSP", name)
		}
	}
}

// TestDocsRender proves the web/docs text templates render with DocView and that
// skill.json stays valid JSON-ish (balanced braces) after substitution.
func TestDocsRender(t *testing.T) {
	docs, err := texttemplate.ParseFS(web.FS, "docs/*")
	if err != nil {
		t.Fatalf("parse docs: %v", err)
	}
	view := DocView{BaseURL: "http://localhost:8080", Host: "localhost:8080", Version: "test", Admins: []string{"mgreau"}, Tools: ToolNames, SkillSHA: "abc", Generated: "2026-09-04T00:00:00Z", KeyExample: "ab_x"}
	for _, name := range []string{"skill.md", "skill.json", "llms.txt", "agents.md"} {
		var buf bytes.Buffer
		if err := docs.ExecuteTemplate(&buf, name, view); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !strings.Contains(buf.String(), "http://localhost:8080") {
			t.Errorf("%s: BaseURL not substituted", name)
		}
		if name == "skill.json" {
			var v map[string]any
			if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
				t.Errorf("skill.json is not valid JSON after rendering: %v\n%s", err, buf.String())
			}
		}
	}
}
