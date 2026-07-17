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

func TestSettingsRequiresAuth(t *testing.T) {
	_, h := newTestServer(t)
	rr := get(h, "/_app/settings")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("unauth settings status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != loginPath {
		t.Fatalf("redirect Location = %q, want %q", loc, loginPath)
	}
}

func TestSettingsFormRenders(t *testing.T) {
	srv, h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/_app/settings", nil)
	req.AddCookie(srv.auth.IssueSessionCookie())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("settings status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, field := range []string{`name="current_password"`, `name="new_password"`, `name="confirm_password"`} {
		if !strings.Contains(body, field) {
			t.Fatalf("settings form missing %s", field)
		}
	}
}

func TestChangePasswordSuccess(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"current_password": {testAdminPassword},
		"new_password":     {"brand-new-pass"},
		"confirm_password": {"brand-new-pass"},
		"csrf_token":       {srv.auth.IssueCSRFToken()},
	}
	rr := postTo(h, "/_app/settings/password", form, srv.auth.IssueSessionCookie())

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("change-password status = %d, want 303", rr.Code)
	}
	hash, _ := auth.AdminPasswordHash(context.Background(), srv.store)
	if !auth.VerifyPassword(hash, "brand-new-pass") {
		t.Fatal("new password does not verify after change")
	}
	if auth.VerifyPassword(hash, testAdminPassword) {
		t.Fatal("old password still verifies after change")
	}
}

func TestChangePasswordWrongCurrent(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"current_password": {"not-the-password"},
		"new_password":     {"brand-new-pass"},
		"confirm_password": {"brand-new-pass"},
		"csrf_token":       {srv.auth.IssueCSRFToken()},
	}
	rr := postTo(h, "/_app/settings/password", form, srv.auth.IssueSessionCookie())

	if rr.Code != http.StatusOK {
		t.Fatalf("wrong-current status = %d, want 200 re-render", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Current password is incorrect") {
		t.Fatal("wrong-current response missing error message")
	}
	hash, _ := auth.AdminPasswordHash(context.Background(), srv.store)
	if !auth.VerifyPassword(hash, testAdminPassword) {
		t.Fatal("password changed despite wrong current password")
	}
}

func TestChangePasswordMismatch(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"current_password": {testAdminPassword},
		"new_password":     {"brand-new-pass"},
		"confirm_password": {"different-pass"},
		"csrf_token":       {srv.auth.IssueCSRFToken()},
	}
	rr := postTo(h, "/_app/settings/password", form, srv.auth.IssueSessionCookie())

	if rr.Code != http.StatusOK {
		t.Fatalf("mismatch status = %d, want 200 re-render", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "do not match") {
		t.Fatal("mismatch response missing error message")
	}
}

func TestChangePasswordTooShort(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"current_password": {testAdminPassword},
		"new_password":     {"short"},
		"confirm_password": {"short"},
		"csrf_token":       {srv.auth.IssueCSRFToken()},
	}
	rr := postTo(h, "/_app/settings/password", form, srv.auth.IssueSessionCookie())

	if rr.Code != http.StatusOK {
		t.Fatalf("too-short status = %d, want 200 re-render", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "at least 8 characters") {
		t.Fatal("too-short response missing error message")
	}
}

func TestChangePasswordSameAsCurrent(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"current_password": {testAdminPassword},
		"new_password":     {testAdminPassword},
		"confirm_password": {testAdminPassword},
		"csrf_token":       {srv.auth.IssueCSRFToken()},
	}
	rr := postTo(h, "/_app/settings/password", form, srv.auth.IssueSessionCookie())

	if rr.Code != http.StatusOK {
		t.Fatalf("same-as-current status = %d, want 200 re-render", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "different from the current password") {
		t.Fatal("same-as-current response missing error message")
	}
}

func TestChangePasswordMissingCSRF(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"current_password": {testAdminPassword},
		"new_password":     {"brand-new-pass"},
		"confirm_password": {"brand-new-pass"},
	}
	rr := postTo(h, "/_app/settings/password", form, srv.auth.IssueSessionCookie())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing-CSRF status = %d, want 403", rr.Code)
	}
}

func TestAPIKeyCreateShowsTokenOnce(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"name":       {"ci-bot"},
		"csrf_token": {srv.auth.IssueCSRFToken()},
	}
	rr := postTo(h, "/_app/settings/api-keys", form, srv.auth.IssueSessionCookie())
	if rr.Code != http.StatusOK {
		t.Fatalf("create key status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// The plaintext token (pp_ prefix) is shown exactly once, on this response.
	if !strings.Contains(body, "pp_") {
		t.Fatal("create response did not show the plaintext token")
	}
	if !strings.Contains(body, "ci-bot") {
		t.Fatal("create response missing key name")
	}

	// The stored key persists (one row), hash-only.
	keys, err := srv.store.ListAPIKeys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListAPIKeys: %v (n=%d)", err, len(keys))
	}
	if strings.HasPrefix(keys[0].KeyHash, "pp_") {
		t.Fatal("stored key_hash unexpectedly looks like a plaintext token")
	}

	// A subsequent settings render must NOT show any plaintext token.
	req := httptest.NewRequest(http.MethodGet, "/_app/settings", nil)
	req.AddCookie(srv.auth.IssueSessionCookie())
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req)
	if strings.Contains(rr2.Body.String(), "pp_") {
		t.Fatal("settings page leaked a plaintext token on a later render")
	}
}

func TestAPIKeyCreateRequiresName(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{"csrf_token": {srv.auth.IssueCSRFToken()}}
	rr := postTo(h, "/_app/settings/api-keys", form, srv.auth.IssueSessionCookie())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "name for the key is required") {
		t.Fatal("missing-name error not shown")
	}
	keys, _ := srv.store.ListAPIKeys(context.Background())
	if len(keys) != 0 {
		t.Fatalf("no key should be created; got %d", len(keys))
	}
}

func TestAPIKeyDelete(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	keys, _ := srv.store.ListAPIKeys(context.Background())
	if len(keys) != 1 {
		t.Fatalf("setup: want 1 key, got %d", len(keys))
	}
	id := keys[0].ID

	form := url.Values{"csrf_token": {srv.auth.IssueCSRFToken()}}
	rr := postTo(h, "/_app/settings/api-keys/"+id+"/delete", form, srv.auth.IssueSessionCookie())
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", rr.Code)
	}
	keys, _ = srv.store.ListAPIKeys(context.Background())
	if len(keys) != 0 {
		t.Fatalf("key not deleted; got %d", len(keys))
	}
	// The revoked token no longer authenticates.
	rr = apiReq(t, h, token, http.MethodGet, "/api/v1/projects", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d, want 401", rr.Code)
	}
}

func TestAPIKeyManagementRequiresCSRF(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{"name": {"x"}} // no csrf_token
	rr := postTo(h, "/_app/settings/api-keys", form, srv.auth.IssueSessionCookie())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}
