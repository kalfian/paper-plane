package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/auth"
)

// postTo posts form as application/x-www-form-urlencoded to path, attaching any
// cookies, and returns the recorder.
func postTo(h http.Handler, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestSetupFormRendersWhenUnconfigured(t *testing.T) {
	_, h := newFreshServer(t)
	rr := get(h, "/_app/setup")
	if rr.Code != http.StatusOK {
		t.Fatalf("setup form status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, field := range []string{`name="csrf_token"`, `name="new_password"`, `name="confirm_password"`} {
		if !strings.Contains(body, field) {
			t.Fatalf("setup form missing %s", field)
		}
	}
}

func TestLoginRedirectsToSetupWhenUnconfigured(t *testing.T) {
	_, h := newFreshServer(t)
	rr := get(h, "/_app/login")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303 redirect", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != setupPath {
		t.Fatalf("redirect Location = %q, want %q", loc, setupPath)
	}
}

func TestSetupSuccess(t *testing.T) {
	srv, h := newFreshServer(t)
	form := url.Values{
		"new_password":     {"correct-horse"},
		"confirm_password": {"correct-horse"},
		"csrf_token":       {srv.auth.IssueCSRFToken()},
	}
	rr := postTo(h, "/_app/setup", form)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("setup status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/_app/" {
		t.Fatalf("redirect Location = %q, want /_app/", loc)
	}
	if !hasSessionCookie(rr.Result().Cookies()) {
		t.Fatal("successful setup did not auto-login (no session cookie)")
	}

	// Password is now configured and the new value authenticates.
	configured, err := auth.PasswordConfigured(context.Background(), srv.store)
	if err != nil || !configured {
		t.Fatalf("PasswordConfigured = %v, %v; want true, nil", configured, err)
	}
	hash, _ := auth.AdminPasswordHash(context.Background(), srv.store)
	if !auth.VerifyPassword(hash, "correct-horse") {
		t.Fatal("stored hash does not verify the chosen password")
	}
}

func TestSetupRejectsShortPassword(t *testing.T) {
	srv, h := newFreshServer(t)
	form := url.Values{
		"new_password":     {"short"},
		"confirm_password": {"short"},
		"csrf_token":       {srv.auth.IssueCSRFToken()},
	}
	rr := postTo(h, "/_app/setup", form)

	if rr.Code != http.StatusOK {
		t.Fatalf("short-password status = %d, want 200 re-render", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "at least 8 characters") {
		t.Fatal("short-password response missing validation message")
	}
	if configured, _ := auth.PasswordConfigured(context.Background(), srv.store); configured {
		t.Fatal("short password should not have been stored")
	}
}

func TestSetupRejectsMismatch(t *testing.T) {
	srv, h := newFreshServer(t)
	form := url.Values{
		"new_password":     {"correct-horse"},
		"confirm_password": {"battery-staple"},
		"csrf_token":       {srv.auth.IssueCSRFToken()},
	}
	rr := postTo(h, "/_app/setup", form)

	if rr.Code != http.StatusOK {
		t.Fatalf("mismatch status = %d, want 200 re-render", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "do not match") {
		t.Fatal("mismatch response missing validation message")
	}
	if configured, _ := auth.PasswordConfigured(context.Background(), srv.store); configured {
		t.Fatal("mismatched password should not have been stored")
	}
}

func TestSetupMissingCSRF(t *testing.T) {
	_, h := newFreshServer(t)
	form := url.Values{
		"new_password":     {"correct-horse"},
		"confirm_password": {"correct-horse"},
	}
	rr := postTo(h, "/_app/setup", form)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing-CSRF status = %d, want 403", rr.Code)
	}
}

// TestSetupGuardedWhenConfigured verifies that once a password exists, the setup
// routes refuse to run: GET redirects to login and POST neither changes the
// password nor logs anyone in.
func TestSetupGuardedWhenConfigured(t *testing.T) {
	srv, h := newTestServer(t) // password already set to testAdminPassword

	rr := get(h, "/_app/setup")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != loginPath {
		t.Fatalf("GET setup when configured = %d %q, want 303 -> %q", rr.Code, rr.Header().Get("Location"), loginPath)
	}

	form := url.Values{
		"new_password":     {"attacker-chosen"},
		"confirm_password": {"attacker-chosen"},
		"csrf_token":       {srv.auth.IssueCSRFToken()},
	}
	rr = postTo(h, "/_app/setup", form)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != loginPath {
		t.Fatalf("POST setup when configured = %d %q, want 303 -> %q", rr.Code, rr.Header().Get("Location"), loginPath)
	}
	if hasSessionCookie(rr.Result().Cookies()) {
		t.Fatal("guarded setup POST must not issue a session")
	}
	hash, _ := auth.AdminPasswordHash(context.Background(), srv.store)
	if auth.VerifyPassword(hash, "attacker-chosen") {
		t.Fatal("guarded setup POST overwrote the existing password")
	}
	if !auth.VerifyPassword(hash, testAdminPassword) {
		t.Fatal("existing password was altered by a guarded setup POST")
	}
}
