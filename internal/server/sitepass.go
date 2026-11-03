package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/kalfian/paper-plane/internal/auth"
	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/store"
)

// Per-site visitor passwords.
//
// A project may carry a password (bcrypt hash in projects.password_hash). While
// set, every request for the site — HTML and assets alike — is answered with a
// small unlock page (HTTP 401) instead of the file, unless the visitor:
//
//   - is signed in to the admin (session cookie): the owner is never prompted; or
//   - holds a pp_site cookie for that site, issued after entering the password.
//
// The unlock cookie is HMAC-signed, scoped to Path=/<slug>/, and bound to the
// project id plus a fingerprint of the current hash (see auth.sitePurpose), so
// changing or removing the password logs every visitor out of that site.
//
// The minimum length is deliberately 1 character (operator's choice): this is a
// light "keep casual visitors out" gate, not account security.

const (
	// sitePasswordMaxLen is bcrypt's input limit (bytes).
	sitePasswordMaxLen = 72
	// siteUnlockPathPrefix is where the unlock form posts: /_app/unlock/<slug>.
	siteUnlockPathPrefix = "/_app/unlock/"
	// maxUnlockBody caps the unlock form body.
	maxUnlockBody = 16 << 10
)

// siteLockData is passed to site_lock.html. See web/templates/CONTRACT.md.
type siteLockData struct {
	CSRFToken string
	SiteName  string // project display name
	Action    string // form action, /_app/unlock/<slug>
	Next      string // where to return after unlocking (same site only)
	Error     string // set after a wrong password
}

// validateSitePassword enforces 1..72 bytes. Passwords are not trimmed.
func validateSitePassword(pw string) error {
	switch {
	case pw == "":
		return errValidation("Password must be at least 1 character.")
	case len(pw) > sitePasswordMaxLen:
		return errValidation("Password must be at most 72 bytes.")
	}
	return nil
}

// setSitePassword sets (pw non-empty) or removes (pw == "") a project's visitor
// password.
func (s *Server) setSitePassword(ctx context.Context, id, pw string) error {
	hash := ""
	if pw != "" {
		if err := validateSitePassword(pw); err != nil {
			return err
		}
		h, err := auth.HashPassword(pw)
		if err != nil {
			s.log.Error("hash site password", "id", id, "error", err)
			return errInternalOp
		}
		hash = h
	}
	if err := s.store.SetProjectPassword(ctx, id, hash); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errProjectNotFound
		}
		s.log.Error("set site password", "id", id, "error", err)
		return errInternalOp
	}
	return nil
}

// siteUnlocked reports whether r may view proj: public sites always, protected
// ones for a signed-in admin or a holder of a valid unlock cookie.
func (s *Server) siteUnlocked(r *http.Request, proj *model.Project) bool {
	if !proj.Protected() {
		return true
	}
	return s.auth.VerifySessionRequest(r) ||
		s.auth.VerifySiteRequest(r, proj.ID, proj.PasswordHash)
}

// renderSiteLock answers with the unlock page. It must never be cached or
// framed, and search engines should not index it.
func (s *Server) renderSiteLock(w http.ResponseWriter, proj *model.Project, next, errMsg string, status int) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	s.rd.render(w, status, "site_lock.html", siteLockData{
		CSRFToken: s.auth.IssueCSRFToken(),
		SiteName:  proj.Name,
		Action:    siteUnlockPathPrefix + proj.Slug,
		Next:      next,
		Error:     errMsg,
	})
}

// safeSiteNext returns next when it is a local URL inside the site, otherwise
// the site root. This keeps the post-unlock redirect from leaving the site.
func safeSiteNext(next, slug string) string {
	root := "/" + slug + "/"
	if strings.HasPrefix(next, root) && !strings.ContainsAny(next, "\\\r\n") {
		return next
	}
	return root
}

// handleSiteUnlock checks a visitor's password and sets the site's unlock
// cookie. Registered (public, CSRF-protected) for POST /_app/unlock/{slug}.
func (s *Server) handleSiteUnlock(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	proj, err := s.store.GetProjectBySlug(r.Context(), slug)
	if err != nil || proj.Status != model.StatusActive {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("unlock lookup slug", "slug", slug, "error", err)
		}
		http.NotFound(w, r)
		return
	}

	next := safeSiteNext(r.FormValue("next"), proj.Slug)
	if s.siteUnlocked(r, proj) {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}

	if !auth.VerifyPassword(proj.PasswordHash, r.FormValue("password")) {
		s.renderSiteLock(w, proj, next, "Incorrect password.", http.StatusUnauthorized)
		return
	}

	http.SetCookie(w, s.auth.IssueSiteCookie(r, proj.ID, proj.PasswordHash, "/"+proj.Slug+"/"))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// limitBody caps the request body at n bytes. It must run before anything
// parses the form (including the CSRF check).
func limitBody(n int64) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// handleProjectPassword sets or changes a project's visitor password from the
// admin edit page. Registered for POST /_app/projects/{id}/password.
func (s *Server) handleProjectPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	pw := r.FormValue("password")
	if pw == "" {
		s.renderEditError(w, p, "Password must be at least 1 character.")
		return
	}
	if err := s.setSitePassword(r.Context(), p.ID, pw); err != nil {
		oe := asOpError(err)
		if oe.Status >= 500 {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		s.renderEditError(w, p, oe.Msg)
		return
	}
	flash := "Password+set."
	if p.Protected() {
		flash = "Password+changed."
	}
	http.Redirect(w, r, "/_app/projects/"+p.ID+"?flash="+flash, http.StatusSeeOther)
}

// handleProjectPasswordRemove makes a protected site public again. Registered
// for POST /_app/projects/{id}/password/remove.
func (s *Server) handleProjectPasswordRemove(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	if err := s.setSitePassword(r.Context(), p.ID, ""); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/_app/projects/"+p.ID+"?flash=Password+removed.", http.StatusSeeOther)
}

// renderEditError re-renders the project edit page with an inline password-form
// error (HTTP 200).
func (s *Server) renderEditError(w http.ResponseWriter, p *model.Project, msg string) {
	s.rd.render(w, http.StatusOK, "project_edit.html", projectEditData{
		CSRFToken:     s.auth.IssueCSRFToken(),
		Project:       s.newProjectView(*p),
		PasswordError: msg,
	})
}
