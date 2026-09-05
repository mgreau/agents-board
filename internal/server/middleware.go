package server

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// middleware wraps next in the fixed chain, outermost first:
//
//  1. recover         -> 500 (JSON under /api and /admin, HTML elsewhere), stack logged
//  2. requestLog      -> slog one line per request (method, path, status, duration, tool header)
//  3. securityHeaders (CSP, Permissions-Policy, nosniff, Referrer-Policy, Origin-Trial): set
//     before anything below can answer, so the 301 and the 421 carry them too
//  4. httpsRedirect   -> 301 to https when X-Forwarded-Proto is exactly "http"
//  5. edgeKey         -> 421 when BOARD_EDGE_KEY is set and X-Board-Edge-Key differs (except /health)
//  6. http.CrossOriginProtection (Go 1.25) — applied by New around the mux, inside this chain
//  7. mux; per-route wrappers apiRead/apiWrite/admin handle body limits, JSON, auth, limits
func (s *Server) middleware(next http.Handler) http.Handler {
	h := next
	h = s.edgeKey(h)
	h = s.httpsRedirect(h)
	h = s.securityHeaders(h)
	h = s.requestLog(h)
	h = s.recoverPanic(h)
	return h
}

// CSP has no inline scripts or styles anywhere; board.js and board.css are the only assets.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Permissions-Policy", "tools=(self)")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		// Never send Origin-Agent-Cluster: ?0 — it disables WebMCP.
		if s.cfg.OTToken != "" {
			h.Set("Origin-Trial", s.cfg.OTToken)
		}
		next.ServeHTTP(w, r)
	})
}

// edgeKey enforces the secret header the Datum edge injects, so the bare *.run.app origin
// cannot be used. Constant-time compare. /health is exempt for Cloud Run probes.
func (s *Server) edgeKey(next http.Handler) http.Handler {
	if s.cfg.EdgeKey == "" {
		return next
	}
	want := []byte(s.cfg.EdgeKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get(HeaderEdgeKey))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			s.writeStatus(w, r, http.StatusMisdirectedRequest, ErrEdge, "Use the public board URL, not the origin.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// httpsRedirect answers plain-HTTP requests that reached us through the edge with a 301 to
// the same host and path over https.
func (s *Server) httpsRedirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Forwarded-Proto") == "http" {
			u := *r.URL
			u.Scheme = "https"
			u.Host = r.Host
			http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestLog writes one structured line per request.
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Duration("dur", time.Since(start)),
			slog.String("tool", r.Header.Get(HeaderTool)),
			slog.String("ua", truncate(r.UserAgent(), 80)),
		)
	})
}

// recoverPanic converts a panic into a 500 without killing the process. http.ErrAbortHandler
// is net/http's documented way to abort a response quietly; it is re-panicked so the server
// handles it as designed instead of logging a stack trace.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			s.log.ErrorContext(r.Context(), "panic", "err", rec, "stack", string(debug.Stack()))
			s.writeStatus(w, r, http.StatusInternalServerError, ErrInternal, "Something broke on our side.")
		}()
		next.ServeHTTP(w, r)
	})
}

// writeStatus picks JSON or HTML for an error depending on the path prefix.
func (s *Server) writeStatus(w http.ResponseWriter, r *http.Request, status int, code, hint string) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/admin/") || r.URL.Path == "/health" {
		writeError(w, status, code, hint)
		return
	}
	s.renderError(w, r, status, hint)
}

// statusWriter captures the status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func nowUTC() time.Time { return time.Now().UTC() }

func stripScheme(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	return u
}
