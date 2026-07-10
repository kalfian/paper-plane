package server

import (
	"log/slog"
	"net/http"

	"github.com/kalfian/paper-plane/internal/auth"
)

// handleSetupForm renders the first-run "set a password" page. If a password is
// already configured, setup is over: redirect to the login page so this route
// can never be used to reset an existing credential without authenticating.
// Registered (public, pre-auth) for GET /_app/setup.
func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	configured, err := auth.PasswordConfigured(r.Context(), s.store)
	if err != nil {
		s.log.Error("check password configured", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if configured {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	s.rd.render(w, http.StatusOK, "setup.html", setupData{
		CSRFToken: s.auth.IssueCSRFToken(),
	})
}

// handleSetupSubmit sets the initial admin password on a fresh instance, then
// logs the operator in and sends them to the dashboard. It refuses once a
// password already exists (guarding against a reset by a later visitor). On a
// validation failure it re-renders the form with an error (HTTP 200).
// Registered (public, pre-auth, CSRF-protected) for POST /_app/setup.
func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	configured, err := auth.PasswordConfigured(r.Context(), s.store)
	if err != nil {
		s.log.Error("check password configured", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if configured {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}

	newPw := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")
	if msg := validateNewPassword(newPw, confirm); msg != "" {
		s.rd.render(w, http.StatusOK, "setup.html", setupData{
			CSRFToken: s.auth.IssueCSRFToken(),
			Error:     msg,
		})
		return
	}

	if err := auth.SetAdminPassword(r.Context(), s.store, newPw); err != nil {
		s.log.Error("set admin password", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Setting the password on a fresh instance proves control, so log the
	// operator straight in rather than bouncing them to the login page.
	http.SetCookie(w, s.auth.IssueSessionCookieForRequest(r))
	http.Redirect(w, r, "/_app/", http.StatusSeeOther)
}
