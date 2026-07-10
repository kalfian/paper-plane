package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/auth"
	"github.com/kalfian/paper-plane/internal/config"
	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/sitefs"
	"github.com/kalfian/paper-plane/internal/store"
)

const testAdminPassword = "s3cret-pass"

// newTestServer builds a Server backed by a temp-file SQLite store with the
// admin password already configured (i.e. first-run setup is complete).
func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv, h := newFreshServer(t)
	if err := auth.SetAdminPassword(context.Background(), srv.store, testAdminPassword); err != nil {
		t.Fatalf("SetAdminPassword: %v", err)
	}
	return srv, h
}

// newFreshServer builds a Server on an empty store with NO admin password set,
// modelling a brand-new instance before first-run setup.
func newFreshServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	st, err := store.NewSQLite(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{Port: "0", DataDir: dir}
	srv, err := New(ctx, cfg, st, sitefs.New(dir), nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv, srv.Routes()
}

func TestHealthz(t *testing.T) {
	_, h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_app/healthz", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Fatalf("healthz body = %q, want ok", rr.Body.String())
	}
}

func TestLoginFormRenders(t *testing.T) {
	_, h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_app/login", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("login form status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Fatal("login form missing csrf_token field")
	}
	if !strings.Contains(body, `name="password"`) {
		t.Fatal("login form missing password field")
	}
}

func TestLoginSuccess(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"password":   {testAdminPassword},
		"csrf_token": {srv.auth.IssueCSRFToken()},
	}
	rr := postForm(h, form)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/_app/" {
		t.Fatalf("redirect Location = %q, want /_app/", loc)
	}
	if !hasSessionCookie(rr.Result().Cookies()) {
		t.Fatal("login success did not set session cookie")
	}
}

func TestLoginWrongPassword(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"password":   {"nope"},
		"csrf_token": {srv.auth.IssueCSRFToken()},
	}
	rr := postForm(h, form)

	if rr.Code != http.StatusOK {
		t.Fatalf("wrong-password status = %d, want 200", rr.Code)
	}
	if hasSessionCookie(rr.Result().Cookies()) {
		t.Fatal("wrong password should not set a session cookie")
	}
	if !strings.Contains(rr.Body.String(), "Incorrect password") {
		t.Fatal("wrong password response missing error message")
	}
}

func TestLoginMissingCSRF(t *testing.T) {
	_, h := newTestServer(t)
	form := url.Values{"password": {testAdminPassword}}
	rr := postForm(h, form)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing-CSRF status = %d, want 403", rr.Code)
	}
}

func TestLoginBadCSRF(t *testing.T) {
	_, h := newTestServer(t)
	form := url.Values{
		"password":   {testAdminPassword},
		"csrf_token": {"totally.invalid"},
	}
	rr := postForm(h, form)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("bad-CSRF status = %d, want 403", rr.Code)
	}
}

func TestDashboardRequiresAuth(t *testing.T) {
	_, h := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_app/", nil))

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("unauth dashboard status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != loginPath {
		t.Fatalf("redirect Location = %q, want %q", loc, loginPath)
	}
}

func TestDashboardWithValidSession(t *testing.T) {
	srv, h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/_app/", nil)
	req.AddCookie(srv.auth.IssueSessionCookie())

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authed dashboard status = %d, want 200", rr.Code)
	}
}

func TestDashboardTamperedSession(t *testing.T) {
	srv, h := newTestServer(t)
	c := srv.auth.IssueSessionCookie()
	c.Value = "x" + c.Value // corrupt signature

	req := httptest.NewRequest(http.MethodGet, "/_app/", nil)
	req.AddCookie(c)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("tampered session status = %d, want 303 redirect", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != loginPath {
		t.Fatalf("redirect Location = %q, want %q", loc, loginPath)
	}
}

func TestLogoutClearsSession(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{"csrf_token": {srv.auth.IssueCSRFToken()}}
	req := httptest.NewRequest(http.MethodPost, "/_app/logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(srv.auth.IssueSessionCookie())

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", rr.Code)
	}
	// Cleared cookie has MaxAge < 0.
	var cleared bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear session cookie")
	}
}

// TestSecurityHeadersOnAdmin verifies the defense-in-depth headers are present
// on admin (/_app/*) responses.
func TestSecurityHeadersOnAdmin(t *testing.T) {
	_, h := newTestServer(t)
	want := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
	}
	for _, path := range []string{"/_app/login", "/_app/"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		for k, v := range want {
			if got := rr.Header().Get(k); got != v {
				t.Fatalf("%s header %s = %q, want %q", path, k, got, v)
			}
		}
	}
}

// TestSecurityHeadersNotOnHostedSite verifies the admin security headers are NOT
// forced onto responses for hosted static sites, leaving per-site policy to the
// site owner.
func TestSecurityHeadersNotOnHostedSite(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	if err := srv.fs.WriteFile(id, "index.html", []byte("<head></head>hi")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rr := get(h, "/demo/")
	if rr.Code != http.StatusOK {
		t.Fatalf("hosted site status = %d, want 200", rr.Code)
	}
	for _, k := range []string{"X-Frame-Options", "X-Content-Type-Options", "Referrer-Policy"} {
		if got := rr.Header().Get(k); got != "" {
			t.Fatalf("hosted site should not set %s, got %q", k, got)
		}
	}
}

// TestLoginSecureCookieViaForwardedProto verifies that a login behind a
// TLS-terminating proxy (X-Forwarded-Proto: https) yields a Secure session
// cookie even when APP_URL is not https.
func TestLoginSecureCookieViaForwardedProto(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"password":   {testAdminPassword},
		"csrf_token": {srv.auth.IssueCSRFToken()},
	}
	req := httptest.NewRequest(http.MethodPost, "/_app/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", rr.Code)
	}
	var sess *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("login did not set a session cookie")
	}
	if !sess.Secure {
		t.Fatal("session cookie not marked Secure despite X-Forwarded-Proto: https")
	}
}

// TestLoginNonSecureCookieByDefault verifies that without https (no APP_URL
// https and no X-Forwarded-Proto), the session cookie is not marked Secure, so
// plain-HTTP local/dev use still works.
func TestLoginNonSecureCookieByDefault(t *testing.T) {
	srv, h := newTestServer(t)
	form := url.Values{
		"password":   {testAdminPassword},
		"csrf_token": {srv.auth.IssueCSRFToken()},
	}
	rr := postForm(h, form)
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Secure {
			t.Fatal("session cookie unexpectedly marked Secure over plain HTTP")
		}
	}
}

func postForm(h http.Handler, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/_app/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func hasSessionCookie(cookies []*http.Cookie) bool {
	for _, c := range cookies {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			return true
		}
	}
	return false
}
