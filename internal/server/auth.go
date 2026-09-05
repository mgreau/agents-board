package server

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mgreau/agents-board/internal/store"
)

// session is what apiRead/apiWrite resolve from the board_sid cookie; nil when signed out.
type session struct {
	Agent   *store.Agent
	Session *store.Session
	Token   string // raw cookie value (needed for TouchSession)
}

// resolveSession reads the board_sid cookie. It returns nil when there is no valid session;
// revoked is true when the cookie was valid but the agent is disabled (403 instead of 401).
// A valid session is touched (sliding expiry) and the cookie re-sent when it was extended.
func (s *Server) resolveSession(w http.ResponseWriter, r *http.Request) (sess *session, revoked bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, false
	}
	st, agent, err := s.store.SessionAgent(r.Context(), c.Value)
	switch {
	case errors.Is(err, store.ErrRevoked):
		return nil, true
	case errors.Is(err, store.ErrNotFound):
		return nil, false
	case err != nil:
		s.log.ErrorContext(r.Context(), "resolve session", "err", err)
		return nil, false
	}
	if extended, _, err := s.store.TouchSession(r.Context(), c.Value); err != nil {
		s.log.WarnContext(r.Context(), "touch session", "err", err)
	} else if extended {
		s.setSessionCookie(w, c.Value)
	}
	return &session{Agent: agent, Session: st, Token: c.Value}, false
}

// requireSession answers 401 not_signed_in (or 403 revoked) and returns false when sess is nil.
func requireSession(w http.ResponseWriter, sess *session, revoked bool) bool {
	if sess != nil {
		return true
	}
	if revoked {
		writeError(w, http.StatusForbidden, ErrRevoked, "This agent was revoked. Ask the board admin for a new key.")
		return false
	}
	writeError(w, http.StatusUnauthorized, ErrNotSignedIn, "Not signed in. Call join with your ab_ key (once per browser profile).")
	return false
}

// setSessionCookie sends board_sid with the fixed attributes. Secure is set even on
// http://localhost: Chrome accepts Secure cookies from localhost, and an expiry-less cookie
// would be dropped by headless Chrome between runs, so Max-Age is always present.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   CookieMaxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// bearerOK compares the Authorization header against the configured admin token in
// constant time.
func bearerOK(r *http.Request, want string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if want == "" || !strings.HasPrefix(h, prefix) {
		return false
	}
	return constantTimeEqual(strings.TrimSpace(h[len(prefix):]), want)
}

// ipHasher turns client addresses into sha256(ip + daily salt) so per-IP limits work
// without storing addresses. The salt is 16 random bytes regenerated when the UTC date
// changes.
type ipHasher struct {
	header string // header carrying the client IP; "" = RemoteAddr
	now    func() time.Time

	mu   sync.Mutex
	day  string
	salt []byte
}

func newIPHasher(header string, now func() time.Time) *ipHasher {
	return &ipHasher{header: header, now: now}
}

// currentSalt returns today's salt, rotating it on a new UTC day.
func (h *ipHasher) currentSalt() []byte {
	day := h.now().UTC().Format("2006-01-02")
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.day != day || len(h.salt) == 0 {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			// crypto/rand failing is fatal for the whole process on every platform we run on;
			// fall back to the date so the limiter keeps working rather than panicking.
			salt = []byte(day + "-fallback-salt-")
		}
		h.salt, h.day = salt, day
	}
	return h.salt
}

// hash returns the hashed client IP for r. There is always a bucket: when the configured
// header is absent the limiter falls back to RemoteAddr rather than failing open.
func (h *ipHasher) hash(r *http.Request) []byte {
	sum := sha256.Sum256(append([]byte(h.clientIP(r)), h.currentSalt()...))
	return sum[:]
}

// clientIP extracts the address string: the configured header when present (values may be
// a list; the first entry is the one the edge set), else RemoteAddr.
func (h *ipHasher) clientIP(r *http.Request) string {
	if h.header != "" {
		v := r.Header.Get(h.header)
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		if v = strings.TrimSpace(v); v != "" {
			if host, _, err := net.SplitHostPort(v); err == nil {
				return host
			}
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
