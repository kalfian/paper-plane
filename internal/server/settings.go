package server

import (
	"log/slog"
	"net/http"

	"github.com/kalfian/paper-plane/internal/auth"
)

// Password policy for the change-password form. bcrypt only considers the first
// 72 bytes of input, so we reject anything longer rather than silently ignore
// the tail.
const (
	minPasswordLen = 8
	maxPasswordLen = 72
)

// settingsData is passed to settings.html. See web/templates/CONTRACT.md.
type settingsData struct {
	CSRFToken string
	Flash     string
	Error     string
}

// handleSettings renders the account settings page (currently just the
// change-password form). Registered for GET /_app/settings.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.rd.render(w, http.StatusOK, "settings.html", settingsData{
		CSRFToken: s.auth.IssueCSRFToken(),
		Flash:     r.URL.Query().Get("flash"),
	})
}

// handleChangePassword verifies the current password, validates the new one,
// and rotates the stored hash. On any validation failure it re-renders the form
// with an error (HTTP 200) and a fresh CSRF token; passwords are never echoed
// back. On success it redirects (303) back to the settings page with a flash.
// The cookie secret is untouched, so the current session (and any others) stay
// valid.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	current := r.FormValue("current_password")
	newPw := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	hash, err := auth.AdminPasswordHash(r.Context(), s.store)
	if err != nil {
		s.log.Error("load admin password hash", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if !auth.VerifyPassword(hash, current) {
		s.renderSettingsError(w, "Current password is incorrect.")
		return
	}
	if msg := validateNewPassword(newPw, confirm); msg != "" {
		s.renderSettingsError(w, msg)
		return
	}
	if auth.VerifyPassword(hash, newPw) {
		s.renderSettingsError(w, "New password must be different from the current password.")
		return
	}

	if err := auth.SetAdminPassword(r.Context(), s.store, newPw); err != nil {
		s.log.Error("set admin password", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/_app/settings?flash=Password+updated.", http.StatusSeeOther)
}

// validateNewPassword returns a friendly error string ("" when valid).
func validateNewPassword(newPw, confirm string) string {
	switch {
	case len(newPw) < minPasswordLen:
		return "New password must be at least 8 characters."
	case len(newPw) > maxPasswordLen:
		return "New password must be at most 72 characters."
	case newPw != confirm:
		return "New password and confirmation do not match."
	default:
		return ""
	}
}

// renderSettingsError re-renders the settings form with an error (HTTP 200) and
// a fresh CSRF token.
func (s *Server) renderSettingsError(w http.ResponseWriter, msg string) {
	s.rd.render(w, http.StatusOK, "settings.html", settingsData{
		CSRFToken: s.auth.IssueCSRFToken(),
		Error:     msg,
	})
}
