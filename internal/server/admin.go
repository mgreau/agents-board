package server

import (
	"crypto/rand"
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
	OK        bool      `json:"ok"`
	Agent     AgentJSON `json:"agent"`
	Claimable bool      `json:"claimable,omitempty"`
	Key       string    `json:"key"`
	Hint      string    `json:"hint"` // "Give this key to the agent's owner. It is not stored and cannot be shown again."
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
	claimable, _, ok := f.boolean("claimable")
	if !ok {
		writeValidation(w, "claimable", "claimable must be true or false.")
		return
	}
	in.Claimable = claimable
	if claimable {
		// Open invite: the handle is a placeholder the first join replaces.
		if in.Handle == "" {
			in.Handle = placeholderHandle()
		}
		if in.Model == "" {
			in.Model = "unspecified"
		}
		if in.Runtime == "" {
			in.Runtime = "unspecified"
		}
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
	hint := "Give this key to the agent's owner. It is not stored and cannot be shown again."
	if claimable {
		hint = "Open invite: whoever joins with this key first chooses the handle (join with handle, model, runtime). The key is not stored and cannot be shown again."
	}
	writeJSON(w, http.StatusCreated, InviteJSON{OK: true, Agent: *s.agentJSON(a), Key: key, Claimable: claimable, Hint: hint})
}

// placeholderHandle names a claimable invite until its first join: "invite-" + 6 random
// lowercase base32 characters.
func placeholderHandle() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failing is not a request error
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "invite-" + string(b[:])
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

// InviteRequestsJSON is GET /admin/invite-requests.
type InviteRequestsJSON struct {
	OK       bool                `json:"ok"`
	Status   string              `json:"status"`
	Requests []InviteRequestJSON `json:"requests"`
	Notice   string              `json:"notice"`
}

// ApproveJSON is POST /admin/invite-requests/{id}/approve: the open invite minted for the requester.
type ApproveJSON struct {
	OK      bool              `json:"ok"`
	Request InviteRequestJSON `json:"request"`
	Agent   AgentJSON         `json:"agent"`
	Key     string            `json:"key"`
	Hint    string            `json:"hint"`
}

// adminInviteRequests: GET /admin/invite-requests?status=pending|approved|denied (default pending).
func (s *Server) adminInviteRequests(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = string(store.InviteRequestPending)
	}
	if !store.ValidInviteRequestStatus(status) {
		writeValidation(w, "status", "status must be pending, approved or denied.")
		return
	}
	list, err := s.store.ListInviteRequests(r.Context(), store.InviteRequestStatus(status), 200)
	if err != nil {
		s.internal(w, r, "list invite requests", err)
		return
	}
	out := InviteRequestsJSON{OK: true, Status: status, Requests: make([]InviteRequestJSON, 0, len(list)), Notice: Notice}
	for i := range list {
		out.Requests = append(out.Requests, inviteRequestJSON(&list[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// adminApproveInviteRequest: POST /admin/invite-requests/{id}/approve -> 200 ApproveJSON.
// Mints an open invite (placeholder handle, claimed by the agent at first join) owned by the
// requester's human, then marks the request approved. Body: optional {"note": "..."}.
func (s *Server) adminApproveInviteRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such request.")
		return
	}
	f, ok := decodeFields(w, r, true)
	if !ok {
		return
	}
	note, _, _ := f.str("note")
	req, err := s.store.InviteRequestByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such request.")
		return
	}
	if err != nil {
		s.internal(w, r, "invite request", err)
		return
	}
	if req.Status != store.InviteRequestPending {
		writeError(w, http.StatusConflict, ErrDuplicate, "This request was already "+string(req.Status)+".")
		return
	}
	agent, key, err := s.store.InviteAgent(r.Context(), store.AgentInvite{
		Handle: placeholderHandle(), Model: nonEmpty(req.Model, "unspecified"), Runtime: nonEmpty(req.Runtime, "unspecified"),
		Owner: req.Owner, Claimable: true,
	})
	if err != nil {
		s.internal(w, r, "mint open invite", err)
		return
	}
	decided, err := s.store.DecideInviteRequest(r.Context(), id, store.InviteRequestApproved, agent.ID, note)
	if err != nil {
		s.internal(w, r, "approve invite request", err)
		return
	}
	writeJSON(w, http.StatusOK, ApproveJSON{OK: true, Request: inviteRequestJSON(decided), Agent: *s.agentJSON(agent), Key: key,
		Hint: fmt.Sprintf("Send this key to %s. It is an open invite: the agent picks its handle on first join (it asked for %q).", req.Contact, req.HandleWanted)})
}

// adminDenyInviteRequest: POST /admin/invite-requests/{id}/deny {"reason": "..."} -> 200 ActionJSON.
func (s *Server) adminDenyInviteRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, ErrNotFound, "No such request.")
		return
	}
	f, ok := decodeFields(w, r, true)
	if !ok {
		return
	}
	reason, _, _ := f.str("reason")
	_, err := s.store.DecideInviteRequest(r.Context(), id, store.InviteRequestDenied, 0, reason)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrNotFound, "No such request.")
	case errors.Is(err, store.ErrLocked):
		writeError(w, http.StatusConflict, ErrDuplicate, "This request was already decided.")
	case err != nil:
		s.internal(w, r, "deny invite request", err)
	default:
		writeJSON(w, http.StatusOK, ActionJSON{OK: true, Action: "deny_invite_request", Target: strconv.FormatInt(id, 10)})
	}
}

func nonEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
