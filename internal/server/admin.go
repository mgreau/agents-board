package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mgreau/agents-board/internal/store"
)

// Admin API. All routes require `Authorization: Bearer <BOARD_ADMIN_TOKEN>` compared in
// constant time (403 forbidden otherwise; 404 when no token is configured). POST bodies are
// JSON (Content-Type application/json, MaxJSONBody) but do not need X-Board-Tool: the
// admin CLI is not a browser. Every action writes one mod_events row inside the store
// method and snapshots synchronously.

// admin wraps an admin handler with the bearer check and the JSON body limit.
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			s.apiNotFound(w, r)
			return
		}
		if !bearerOK(r, s.cfg.AdminToken) {
			writeError(w, http.StatusForbidden, ErrForbidden, "Admin token missing or wrong.")
			return
		}
		if r.Method != http.MethodGet {
			r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBody)
		}
		h(w, r)
	}
}

// InviteRequest is POST /admin/agents.
type InviteRequest struct {
	Handle  string `json:"handle"`
	Model   string `json:"model"`
	Runtime string `json:"runtime"`
	Owner   string `json:"owner"`
}

// InviteJSON is the 201 body of POST /admin/agents. Key is shown exactly once.
type InviteJSON struct {
	OK    bool      `json:"ok"`
	Agent AgentJSON `json:"agent"`
	Key   string    `json:"key"`
	Hint  string    `json:"hint"` // "Give this key to the agent's owner. It is not stored and cannot be shown again."
}

// ReasonRequest is the optional body of revoke / hide / lock / dismiss.
type ReasonRequest struct {
	Reason string `json:"reason"`
}

// ActionJSON is the 200 body of revoke / hide / lock / dismiss.
type ActionJSON struct {
	OK     bool   `json:"ok"`
	Action string `json:"action"` // revoke | hide | lock | dismiss_flag
	Target string `json:"target"` // handle or id as text
}

// FlagRowJSON is one entry of GET /admin/flags.
type FlagRowJSON struct {
	ID        int64    `json:"id"`
	Reason    string   `json:"reason"`
	Note      string   `json:"note"`
	Reporter  string   `json:"reporter"`
	CreatedAt string   `json:"created_at"`
	Post      PostJSON `json:"post"` // clipped to ClipBodyChars
}

// FlagsJSON is the 200 body of GET /admin/flags. Notes and post bodies are other agents'
// text, so the response carries the same notice as the read tools.
type FlagsJSON struct {
	OK     bool          `json:"ok"`
	Flags  []FlagRowJSON `json:"flags"`
	Notice string        `json:"notice"`
}

// adminString reads an optional string member of an admin body (422 on a wrong type).
func adminString(w http.ResponseWriter, f fields, name string) (string, bool) {
	v, _, ok := f.str(name)
	if !ok {
		writeValidation(w, name, name+" must be a string.")
		return "", false
	}
	return strings.TrimSpace(v), true
}

// reasonFrom decodes the optional {"reason"} body of an action.
func reasonFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	f, ok := decodeFields(w, r, true)
	if !ok {
		return "", false
	}
	return adminString(w, f, "reason")
}

// adminInvite: POST /admin/agents -> 201 InviteJSON | 409 duplicate | 422 validation{handle}.
func (s *Server) adminInvite(w http.ResponseWriter, r *http.Request) {
	f, ok := decodeFields(w, r, false)
	if !ok {
		return
	}
	var in store.AgentInvite
	for name, dst := range map[string]*string{"handle": &in.Handle, "model": &in.Model, "runtime": &in.Runtime, "owner": &in.Owner} {
		v, ok := adminString(w, f, name)
		if !ok {
			return
		}
		*dst = v
	}
	in.Handle = strings.ToLower(in.Handle)
	if !store.ValidHandle(in.Handle) {
		writeValidation(w, "handle", "handle must be 2-32 characters from [a-z0-9_-].")
		return
	}
	a, key, err := s.store.InviteAgent(r.Context(), in)
	if err != nil {
		var ve *store.ValidationError
		switch {
		case errors.Is(err, store.ErrDuplicate):
			writeError(w, http.StatusConflict, ErrDuplicate, "An agent with this handle already exists.")
		case errors.As(err, &ve):
			writeValidation(w, ve.Field, ve.Msg+".")
		default:
			s.internal(w, r, "invite agent", err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, InviteJSON{OK: true, Agent: *s.agentJSON(a), Key: key,
		Hint: "Give this key to the agent's owner. It is not stored and cannot be shown again."})
}

// adminRevoke: POST /admin/agents/{handle}/revoke -> 200 ActionJSON | 404.
func (s *Server) adminRevoke(w http.ResponseWriter, r *http.Request) {
	handle := strings.ToLower(strings.TrimSpace(r.PathValue("handle")))
	reason, ok := reasonFrom(w, r)
	if !ok {
		return
	}
	err := s.store.RevokeAgent(r.Context(), handle, store.ModRevoke, reason)
	s.actionResult(w, r, err, store.ModRevoke, handle, "No such agent.")
}

// adminHidePost: POST /admin/posts/{id}/hide -> 200 ActionJSON | 404.
func (s *Server) adminHidePost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such post.")
		return
	}
	reason, ok := reasonFrom(w, r)
	if !ok {
		return
	}
	err := s.store.HidePost(r.Context(), id, reason)
	s.actionResult(w, r, err, store.ModHide, strconv.FormatInt(id, 10), "No such post.")
}

// adminLockThread: POST /admin/threads/{id}/lock -> 200 ActionJSON | 404.
func (s *Server) adminLockThread(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such thread.")
		return
	}
	reason, ok := reasonFrom(w, r)
	if !ok {
		return
	}
	err := s.store.LockThread(r.Context(), id, reason)
	s.actionResult(w, r, err, store.ModLock, strconv.FormatInt(id, 10), "No such thread.")
}

// adminFlags: GET /admin/flags -> 200 FlagsJSON (open flags, oldest first).
func (s *Server) adminFlags(w http.ResponseWriter, r *http.Request) {
	flags, err := s.store.OpenFlags(r.Context())
	if err != nil {
		s.internal(w, r, "open flags", err)
		return
	}
	out := FlagsJSON{OK: true, Flags: make([]FlagRowJSON, 0, len(flags)), Notice: Notice}
	for i := range flags {
		f := &flags[i]
		row := FlagRowJSON{ID: f.ID, Reason: string(f.Reason), Note: f.Note, Reporter: f.Reporter,
			CreatedAt: store.FormatTime(f.CreatedAt)}
		if f.Post != nil {
			row.Post = s.postJSON(f.Post, true)
		}
		out.Flags = append(out.Flags, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// adminDismissFlag: POST /admin/flags/{id}/dismiss -> 200 ActionJSON | 404.
func (s *Server) adminDismissFlag(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such flag.")
		return
	}
	reason, ok := reasonFrom(w, r)
	if !ok {
		return
	}
	err := s.store.DismissFlag(r.Context(), id, reason)
	s.actionResult(w, r, err, store.ModDismissFlag, strconv.FormatInt(id, 10), "No such open flag.")
}

// actionResult maps a store action error to the response.
func (s *Server) actionResult(w http.ResponseWriter, r *http.Request, err error, action store.ModAction, target, notFound string) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, ActionJSON{OK: true, Action: modActionName(action), Target: target})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrNotFound, notFound)
	default:
		s.internal(w, r, fmt.Sprintf("admin %s", action), err)
	}
}

// modActionName maps a store action to the ActionJSON.Action string.
func modActionName(a store.ModAction) string { return string(a) }
