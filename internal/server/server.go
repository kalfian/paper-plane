// Package server wires configuration, storage, and auth into an http.Handler.
// Handlers are kept thin: they parse input, call the store/auth boundaries, and
// render. All dependencies are injected via the Server struct (no globals).
package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/kalfian/paper-plane/internal/auth"
	"github.com/kalfian/paper-plane/internal/config"
	"github.com/kalfian/paper-plane/internal/sitefs"
	"github.com/kalfian/paper-plane/internal/store"
	"github.com/kalfian/paper-plane/web"
)

// Server holds the injected dependencies and request handlers.
type Server struct {
	cfg   config.Config
	store store.Store
	fs    sitefs.FileStore
	auth  *auth.Manager
	rd    *renderer
	log   *slog.Logger
}

// New bootstraps auth settings (idempotent), loads the cookie secret, builds
// the auth manager and template renderer, and returns a ready Server. It does
// not start listening; call Routes to obtain the handler.
func New(ctx context.Context, cfg config.Config, st store.Store, fs sitefs.FileStore, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}

	if err := auth.Bootstrap(ctx, st); err != nil {
		return nil, err
	}
	secret, err := auth.CookieSecret(ctx, st)
	if err != nil {
		return nil, err
	}
	rd, err := newRenderer()
	if err != nil {
		return nil, err
	}

	secure := strings.HasPrefix(cfg.AppURL, "https://")
	return &Server{
		cfg:   cfg,
		store: st,
		fs:    fs,
		auth:  auth.NewManager(secret, secure),
		rd:    rd,
		log:   log,
	}, nil
}

// Routes builds the HTTP handler: registers routes on a ServeMux (Go 1.22
// method+path patterns) and wraps everything with recover + logging. Auth and
// CSRF are applied per-route.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /_app/healthz", s.handleHealthz)
	mux.HandleFunc("GET /_app/login", s.handleLoginForm)

	// Admin static assets (CSS + vendored htmx). Public: the login page needs
	// its stylesheet before authentication. Served from the embedded FS whose
	// root holds the "static" directory, so /_app/static/app.css maps to
	// static/app.css. Kept under /_app/ so it never shadows a site slug.
	mux.Handle("GET /_app/static/", cacheStatic(http.StripPrefix("/_app/", http.FileServerFS(web.Static))))
	mux.Handle("POST /_app/login", s.csrf(http.HandlerFunc(s.handleLoginSubmit)))

	// First-run setup (public, pre-auth): choose the initial admin password.
	// Both handlers self-guard, redirecting to login once a password exists.
	mux.HandleFunc("GET /_app/setup", s.handleSetupForm)
	mux.Handle("POST /_app/setup", s.csrf(http.HandlerFunc(s.handleSetupSubmit)))

	// Authenticated admin pages (GET) and mutations (POST + CSRF).
	get := func(h http.HandlerFunc) http.Handler { return chain(h, s.requireAuth) }
	post := func(h http.HandlerFunc) http.Handler { return chain(h, s.requireAuth, s.csrf) }

	mux.Handle("POST /_app/logout", post(s.handleLogout))

	// Account settings.
	mux.Handle("GET /_app/settings", get(s.handleSettings))
	mux.Handle("POST /_app/settings/password", post(s.handleChangePassword))

	// Project management.
	mux.Handle("GET /_app/{$}", get(s.handleProjectsList))
	mux.Handle("GET /_app/projects", get(s.handleProjectsList))
	mux.Handle("GET /_app/projects/new", get(s.handleProjectNew))
	mux.Handle("POST /_app/projects", post(s.handleProjectCreate))
	mux.Handle("GET /_app/projects/{id}", get(s.handleProjectEdit))
	mux.Handle("POST /_app/projects/{id}", post(s.handleProjectUpdate))
	mux.Handle("POST /_app/projects/{id}/unlink", post(s.handleProjectUnlink))
	mux.Handle("POST /_app/projects/{id}/relink", post(s.handleProjectRelink))
	mux.Handle("POST /_app/projects/{id}/delete", post(s.handleProjectDelete))

	// File management.
	mux.Handle("GET /_app/projects/{id}/files", get(s.handleFilesList))
	mux.Handle("POST /_app/projects/{id}/files", post(s.handleFilesUpload))
	mux.Handle("GET /_app/projects/{id}/files/edit", get(s.handleFileEdit))
	mux.Handle("POST /_app/projects/{id}/files/save", post(s.handleFileSave))
	mux.Handle("POST /_app/projects/{id}/files/delete", post(s.handleFileDelete))
	mux.Handle("GET /_app/projects/{id}/files/rename", get(s.handleFileRenameForm))
	mux.Handle("POST /_app/projects/{id}/files/rename", post(s.handleFileRename))
	mux.Handle("POST /_app/projects/{id}/files/index", post(s.handleFileSetIndex))

	// Account settings: API key management.
	mux.Handle("POST /_app/settings/api-keys", post(s.handleAPIKeyCreate))
	mux.Handle("POST /_app/settings/api-keys/{id}/delete", post(s.handleAPIKeyDelete))

	// Any other /_app/* GET is an unknown admin path → 404 (still auth-gated),
	// so it is never mistaken for a site slug by the root handler below.
	mux.Handle("GET /_app/", get(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))

	// REST API (/api/v1/*): JSON, bearer API-key auth (no cookie/CSRF). These
	// patterns are more specific than the "GET /" site fallback, so they take
	// precedence; "api" is a reserved slug so no site can shadow them.
	apiKey := func(h http.HandlerFunc) http.Handler { return chain(h, s.requireAPIKey) }

	mux.HandleFunc("GET /api/v1", s.handleAPIIndex)
	mux.HandleFunc("GET /api/v1/", s.handleAPIIndex)

	mux.Handle("GET /api/v1/projects", apiKey(s.handleAPIProjectsList))
	mux.Handle("POST /api/v1/projects", apiKey(s.handleAPIProjectCreate))
	mux.Handle("GET /api/v1/projects/{id}", apiKey(s.handleAPIProjectGet))
	mux.Handle("PATCH /api/v1/projects/{id}", apiKey(s.handleAPIProjectUpdate))
	mux.Handle("DELETE /api/v1/projects/{id}", apiKey(s.handleAPIProjectDelete))

	mux.Handle("GET /api/v1/projects/{id}/files", apiKey(s.handleAPIFilesList))
	mux.Handle("GET /api/v1/projects/{id}/files/{path...}", apiKey(s.handleAPIFileGet))
	mux.Handle("PUT /api/v1/projects/{id}/files/{path...}", apiKey(s.handleAPIFilePut))
	mux.Handle("DELETE /api/v1/projects/{id}/files/{path...}", apiKey(s.handleAPIFileDelete))

	// Fallback: everything else is a static-site request resolved by slug. The
	// /_app/* patterns above are more specific and take precedence.
	mux.HandleFunc("GET /", s.handleServeSite)

	return chain(mux, s.recoverPanic, s.logging, s.secureHeaders)
}

// cacheStatic tells browsers to revalidate embedded admin assets before use.
// The asset URLs carry no content hash, so a fixed max-age would serve stale
// CSS/JS after an in-place update (until the TTL expired) — a real foot-gun that
// forced users to hard-refresh. "no-cache" still lets the browser cache the
// bytes, but it must revalidate via ETag/Last-Modified (http.FileServerFS sets
// both), so an unchanged asset costs a cheap 304 and a changed one is fetched
// immediately. No hard refresh required.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// handleHealthz reports liveness without auth.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleLoginForm renders the login page with a fresh CSRF token. On a fresh
// instance (no password configured yet) it redirects to first-run setup.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	configured, err := auth.PasswordConfigured(r.Context(), s.store)
	if err != nil {
		s.log.Error("check password configured", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !configured {
		http.Redirect(w, r, setupPath, http.StatusSeeOther)
		return
	}
	s.rd.render(w, http.StatusOK, "login.html", loginData{
		CSRFToken: s.auth.IssueCSRFToken(),
	})
}

// handleLoginSubmit verifies the password and, on success, sets the session
// cookie and redirects to the dashboard. On failure it re-renders the form with
// an error (HTTP 200) and a fresh CSRF token.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	password := r.FormValue("password")

	configured, err := auth.PasswordConfigured(r.Context(), s.store)
	if err != nil {
		s.log.Error("check password configured", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !configured {
		http.Redirect(w, r, setupPath, http.StatusSeeOther)
		return
	}

	hash, err := auth.AdminPasswordHash(r.Context(), s.store)
	if err != nil {
		s.log.Error("load admin password hash", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if !auth.VerifyPassword(hash, password) {
		s.rd.render(w, http.StatusOK, "login.html", loginData{
			CSRFToken: s.auth.IssueCSRFToken(),
			Error:     "Incorrect password.",
		})
		return
	}

	http.SetCookie(w, s.auth.IssueSessionCookieForRequest(r))
	http.Redirect(w, r, "/_app/", http.StatusSeeOther)
}

// handleLogout clears the session and redirects to the login page.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.auth.ClearSessionCookie())
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}
