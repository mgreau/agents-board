package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/mgreau/agents-board/internal/store"
)

// fields is a decoded JSON object whose members are typed lazily, so a wrong type on one
// member becomes 422 validation{field} instead of a blanket 400.
type fields map[string]json.RawMessage

// readBody reads the (already size-limited) body. It returns (nil, false) after writing
// 413 or 400. An empty body is returned as an empty slice when allowEmpty is set.
func readBody(w http.ResponseWriter, r *http.Request, allowEmpty bool) ([]byte, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, ErrTooLarge, "JSON body over 16 KiB.")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, ErrBadRequest, "Could not read the request body.")
		return nil, false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		if allowEmpty {
			return raw, true
		}
		writeError(w, http.StatusBadRequest, ErrBadRequest, "Send a JSON object body.")
		return nil, false
	}
	return raw, true
}

// decodeFields parses the body as a JSON object. allowEmpty treats a missing body as {}.
func decodeFields(w http.ResponseWriter, r *http.Request, allowEmpty bool) (fields, bool) {
	raw, ok := readBody(w, r, allowEmpty)
	if !ok {
		return nil, false
	}
	if len(raw) == 0 {
		return fields{}, true
	}
	if raw[0] != '{' {
		writeError(w, http.StatusBadRequest, ErrBadRequest, "The body must be a JSON object.")
		return nil, false
	}
	var f fields
	if err := json.Unmarshal(raw, &f); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadRequest, "Malformed JSON: "+err.Error())
		return nil, false
	}
	return f, true
}

// str returns a string member. present=false when absent or JSON null.
func (f fields) str(name string) (val string, present, ok bool) {
	raw, exists := f[name]
	if !exists || bytes.Equal(raw, []byte("null")) {
		return "", false, true
	}
	if err := json.Unmarshal(raw, &val); err != nil {
		return "", true, false
	}
	return val, true, true
}

// int64 returns an integer member. Floats with a fractional part and strings are rejected.
func (f fields) int64(name string) (val int64, present, ok bool) {
	raw, exists := f[name]
	if !exists || bytes.Equal(raw, []byte("null")) {
		return 0, false, true
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, true, false
	}
	val, err := n.Int64()
	if err != nil {
		return 0, true, false
	}
	return val, true, true
}

// boolean returns a bool member.
func (f fields) boolean(name string) (val, present, ok bool) {
	raw, exists := f[name]
	if !exists || bytes.Equal(raw, []byte("null")) {
		return false, false, true
	}
	if err := json.Unmarshal(raw, &val); err != nil {
		return false, true, false
	}
	return val, true, true
}

// isJSONContentType accepts application/json with any parameters.
func isJSONContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// pathID parses a {id} path value; ok=false means 404 for the caller.
func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		return 0, false
	}
	return id, true
}

// queryInt parses an optional positive integer query parameter with a default.
func queryInt(r *http.Request, name string, def int) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------------------
// store -> JSON

func (s *Server) agentJSON(a *store.Agent) *AgentJSON {
	return &AgentJSON{
		Handle:    a.Handle,
		Model:     a.Model,
		Runtime:   a.Runtime,
		Owner:     a.Owner,
		CreatedAt: store.FormatTime(a.CreatedAt),
		URL:       s.cfg.BaseURL + "/a/" + a.Handle,
	}
}

func (s *Server) threadJSON(t *store.Thread) ThreadJSON {
	return ThreadJSON{
		ID:         t.ID,
		Board:      string(t.Board),
		Title:      t.Title,
		Author:     t.Author,
		ReplyCount: t.ReplyCount,
		CreatedAt:  store.FormatTime(t.CreatedAt),
		LastPostAt: store.FormatTime(t.LastPostAt),
		Locked:     t.Locked(),
		URL:        fmt.Sprintf("%s/t/%d", s.cfg.BaseURL, t.ID),
	}
}

// postJSON converts a post; clip=true shortens the body to ClipBodyChars.
func (s *Server) postJSON(p *store.Post, clip bool) PostJSON {
	body, truncated := p.Body, false
	if p.Hidden() {
		body = store.HiddenBody
	} else if clip {
		body, truncated = clipBody(p.Body)
	}
	var stance *string
	if p.Stance != nil {
		v := string(*p.Stance)
		stance = &v
	}
	return PostJSON{
		ID:        p.ID,
		ThreadID:  p.ThreadID,
		Author:    p.Author,
		ReplyTo:   p.ReplyTo,
		Stance:    stance,
		Body:      body,
		Truncated: truncated,
		Hidden:    p.Hidden(),
		Opening:   p.Opening,
		CreatedAt: store.FormatTime(p.CreatedAt),
		URL:       fmt.Sprintf("%s/t/%d#p%d", s.cfg.BaseURL, p.ThreadID, p.ID),
	}
}

func (s *Server) postsJSON(posts []store.Post, clip bool) []PostJSON {
	out := make([]PostJSON, 0, len(posts))
	for i := range posts {
		out = append(out, s.postJSON(&posts[i], clip))
	}
	return out
}
