package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// loginPath is where unauthenticated users are sent.
const loginPath = "/_app/login"

// setupPath is the first-run password-setup page, used before any admin
// password has been configured.
const setupPath = "/_app/setup"

// csrfFormField is the form field / header the CSRF token is read from.
const (
	csrfFormField = "csrf_token"
	csrfHeader    = "X-CSRF-Token"
)

// middleware is a standard http.Handler decorator.
type middleware func(http.Handler) http.Handler

// chain applies middlewares to h in order, so the first listed runs outermost.
func chain(h http.Handler, mws ...middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// requireAuth redirects to the login page when the request has no valid
// session cookie.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.VerifySessionRequest(r) {
			http.Redirect(w, r, loginPath, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf rejects unsafe (mutating) requests that lack a valid CSRF token. Safe
// methods (GET, HEAD, OPTIONS) pass through. The token is read from the
// X-CSRF-Token header (for htmx) or the csrf_token form field.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
			next.ServeHTTP(w, r)
			return
		}

		token := r.Header.Get(csrfHeader)
		if token == "" {
			token = r.FormValue(csrfFormField)
		}
		if !s.auth.VerifyCSRFToken(token) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secureHeaders adds defense-in-depth response headers to admin (/_app/*)
// responses only: X-Frame-Options (anti-clickjacking), X-Content-Type-Options
// (no MIME sniffing), and Referrer-Policy. It is intentionally NOT applied to
// hosted static sites (served under /<slug>/) so per-site header policy stays
// with the site owner. No Content-Security-Policy is set, since a strict CSP
// would break the inline/htmx-driven admin UI and is out of scope here.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_app/") {
			h := w.Header()
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
		}
		next.ServeHTTP(w, r)
	})
}

// recoverPanic converts a panic in a handler into a 500 response and logs the
// stack, so one bad request cannot take down the server.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered",
					slog.Any("error", rec),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logging logs one line per request with method, path, status, and duration.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.log.Info("request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Duration("dur", time.Since(start)),
		)
	})
}

// statusWriter captures the response status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}
