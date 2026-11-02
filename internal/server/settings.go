package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kalfian/paper-plane/internal/apikey"
	"github.com/kalfian/paper-plane/internal/auth"
	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/store"
)

// maxAPIKeyNameLen caps the human-friendly label on an API key.
const maxAPIKeyNameLen = 60

// Password policy for the change-password form. bcrypt only considers the first
// 72 bytes of input, so we reject anything longer rather than silently ignore
// the tail.
const (
	minPasswordLen = 8
	maxPasswordLen = 72
)

// apiKeyView is a single row in the API keys table. It never carries plaintext:
// the raw token is shown once (NewKey) immediately after creation only.
type apiKeyView struct {
	ID         string
	Name       string
	Scope      string // "all" | "api" | "mcp"
	ScopeLabel string // human-readable scope
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// settingsData is passed to settings.html. See web/templates/CONTRACT.md.
type settingsData struct {
	CSRFToken string
	Flash     string
	Error     string
	// API keys section.
	APIKeys []apiKeyView
	// NewKey holds a freshly-created plaintext token, shown exactly once. Empty
	// on every other render.
	NewKey string
	// KeyError is an error specific to the API keys section (kept separate from
	// the change-password Error so each form shows its own message).
	KeyError string

	// NewKeyScope is the scope of NewKey; NewKeyMCP reports whether it may call
	// the MCP endpoint (and so whether the MCP setup snippets are shown).
	NewKeyScope string
	NewKeyMCP   bool
	// MCPURL is the absolute MCP endpoint URL (always set).
	MCPURL string
	// Ready-to-paste MCP client setup for NewKey; empty unless NewKeyMCP.
	MCPClaudeCommand string
	MCPJSONConfig    string
	MCPBridgeCommand string
}

// handleSettings renders the account settings page (change-password form + API
// keys). Registered for GET /_app/settings.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	data := settingsData{
		CSRFToken: s.auth.IssueCSRFToken(),
		Flash:     r.URL.Query().Get("flash"),
	}
	s.loadAPIKeys(r, &data)
	s.rd.render(w, http.StatusOK, "settings.html", data)
}

// loadAPIKeys populates data.APIKeys from the store, logging (but not failing)
// on error so the rest of the settings page still renders.
func (s *Server) loadAPIKeys(r *http.Request, data *settingsData) {
	data.MCPURL = s.mcpURL(r)
	keys, err := s.store.ListAPIKeys(r.Context())
	if err != nil {
		s.log.Error("list api keys", slog.Any("error", err))
		return
	}
	views := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, apiKeyView{
			ID:         k.ID,
			Name:       k.Name,
			Scope:      string(k.Scope),
			ScopeLabel: k.Scope.Label(),
			CreatedAt:  k.CreatedAt,
			LastUsedAt: k.LastUsedAt,
		})
	}
	data.APIKeys = views
}

// handleAPIKeyCreate generates a new API key, stores only its hash, and
// re-renders the settings page showing the plaintext token exactly once.
// Registered for POST /_app/settings/api-keys.
func (s *Server) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	switch {
	case name == "":
		s.renderKeyError(w, r, "A name for the key is required.")
		return
	case len(name) > maxAPIKeyNameLen:
		s.renderKeyError(w, r, "Key name is too long.")
		return
	}

	scope := model.KeyScope(strings.TrimSpace(r.FormValue("scope")))
	if scope == "" {
		scope = model.ScopeAll
	}
	if !scope.Valid() {
		s.renderKeyError(w, r, "Choose a valid scope.")
		return
	}

	token, hash := apikey.Generate()
	k := &model.APIKey{ID: store.NewID(), Name: name, KeyHash: hash, Scope: scope}
	if err := s.store.CreateAPIKey(r.Context(), k); err != nil {
		if errors.Is(err, store.ErrKeyExists) {
			// Astronomically unlikely hash collision; ask the user to retry.
			s.renderKeyError(w, r, "Could not create the key, please try again.")
			return
		}
		s.log.Error("create api key", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := settingsData{
		CSRFToken:   s.auth.IssueCSRFToken(),
		NewKey:      token,
		NewKeyScope: string(scope),
		NewKeyMCP:   scope.AllowsMCP(),
	}
	s.loadAPIKeys(r, &data)
	if data.NewKeyMCP {
		data.MCPClaudeCommand, data.MCPJSONConfig, data.MCPBridgeCommand = mcpSnippets(data.MCPURL, s.baseURL(r), token)
	}
	s.rd.render(w, http.StatusOK, "settings.html", data)
}

// baseURL is the instance's public base URL: APP_URL when configured,
// otherwise derived from the request (honouring X-Forwarded-Proto).
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.AppURL != "" {
		return s.cfg.AppURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// mcpURL is the absolute URL of the MCP endpoint.
func (s *Server) mcpURL(r *http.Request) string { return s.baseURL(r) + mcpPath }

// mcpSnippets builds copy-paste client setup for a freshly created token: a
// Claude Code command (remote HTTP), a generic JSON config, and the local stdio
// bridge (which adds upload_local_path).
func mcpSnippets(mcpURL, baseURL, token string) (claudeCmd, jsonCfg, bridgeCmd string) {
	claudeCmd = fmt.Sprintf(`claude mcp add --transport http paperplane %s --header "Authorization: Bearer %s"`, mcpURL, token)
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"paperplane": map[string]any{
				"type":    "http",
				"url":     mcpURL,
				"headers": map[string]string{"Authorization": "Bearer " + token},
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	jsonCfg = string(b)
	bridgeCmd = fmt.Sprintf(`claude mcp add paperplane-local -e PAPERPLANE_URL=%s -e PAPERPLANE_TOKEN=%s -- paperplane mcp`, baseURL, token)
	return claudeCmd, jsonCfg, bridgeCmd
}

// handleAPIKeyDelete revokes (deletes) an API key by id. Registered for POST
// /_app/settings/api-keys/{id}/delete.
func (s *Server) handleAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteAPIKey(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, "/_app/settings?flash=Key+not+found.", http.StatusSeeOther)
			return
		}
		s.log.Error("delete api key", "id", id, slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/_app/settings?flash=API+key+revoked.", http.StatusSeeOther)
}

// renderKeyError re-renders the settings page with a key-section error (HTTP 200)
// and a fresh CSRF token, preserving the existing key list.
func (s *Server) renderKeyError(w http.ResponseWriter, r *http.Request, msg string) {
	data := settingsData{
		CSRFToken: s.auth.IssueCSRFToken(),
		KeyError:  msg,
	}
	s.loadAPIKeys(r, &data)
	s.rd.render(w, http.StatusOK, "settings.html", data)
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
		s.renderSettingsError(w, r, "Current password is incorrect.")
		return
	}
	if msg := validateNewPassword(newPw, confirm); msg != "" {
		s.renderSettingsError(w, r, msg)
		return
	}
	if auth.VerifyPassword(hash, newPw) {
		s.renderSettingsError(w, r, "New password must be different from the current password.")
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
func (s *Server) renderSettingsError(w http.ResponseWriter, r *http.Request, msg string) {
	data := settingsData{
		CSRFToken: s.auth.IssueCSRFToken(),
		Error:     msg,
	}
	s.loadAPIKeys(r, &data)
	s.rd.render(w, http.StatusOK, "settings.html", data)
}
