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
