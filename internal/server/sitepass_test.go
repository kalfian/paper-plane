package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/auth"
	"github.com/kalfian/paper-plane/internal/model"
)

// protectSite seeds an active project with an index page and a stylesheet and
// sets its visitor password.
func protectSite(t *testing.T, srv *Server, slug, pw string) string {
	t.Helper()
	id := seedProject(t, srv, slug, model.StatusActive)
	if err := srv.fs.WriteFile(id, "index.html", []byte("<html><head></head><body>SECRET-"+slug+"</body></html>")); err != nil {
		t.Fatal(err)
	}
	if err := srv.fs.WriteFile(id, "app.css", []byte("body{color:red}")); err != nil {
		t.Fatal(err)
	}
	if pw != "" {
		if err := srv.setSitePassword(context.Background(), id, pw); err != nil {
			t.Fatalf("setSitePassword: %v", err)
		}
	}
	return id
}

// unlock posts the visitor unlock form (with a valid CSRF token).
func unlock(srv *Server, h http.Handler, slug, pw, next string) *httptest.ResponseRecorder {
	form := url.Values{"password": {pw}, "next": {next}, "csrf_token": {srv.auth.IssueCSRFToken()}}
	req := httptest.NewRequest(http.MethodPost, siteUnlockPathPrefix+slug, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func getWith(h http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func siteCookie(t *testing.T, rr *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SiteCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in response (status %d)", auth.SiteCookieName, rr.Code)
	return nil
}

func TestProtectedSiteShowsLockPage(t *testing.T) {
	srv, h := newTestServer(t)
	protectSite(t, srv, "demo", "7")

	for _, path := range []string{"/demo/", "/demo/index.html", "/demo/app.css", "/demo/missing.png"} {
		rr := get(h, path)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", path, rr.Code)
		}
		body := rr.Body.String()
		if strings.Contains(body, "SECRET-demo") || strings.Contains(body, "color:red") {
			t.Fatalf("%s: protected content leaked: %s", path, body)
		}
		if !strings.Contains(body, `action="/_app/unlock/demo"`) {
			t.Fatalf("%s: lock page missing unlock form: %s", path, body)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}
	// The original URL is carried through as "next".
	if rr := get(h, "/demo/app.css?v=2"); !strings.Contains(rr.Body.String(), `value="/demo/app.css?v=2"`) {
		t.Fatalf("lock page missing next: %s", rr.Body.String())
	}
	// The no-slash redirect still happens first (no content involved).
	if rr := get(h, "/demo"); rr.Code != http.StatusMovedPermanently {
		t.Fatalf("/demo: status = %d, want 301", rr.Code)
	}
}

func TestUnlockWithOneCharPassword(t *testing.T) {
	srv, h := newTestServer(t)
	protectSite(t, srv, "demo", "7")

	rr := unlock(srv, h, "demo", "8", "/demo/")
	if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "Incorrect password.") {
		t.Fatalf("wrong password: %d %s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SiteCookieName {
			t.Fatal("wrong password must not set the unlock cookie")
		}
	}

	rr = unlock(srv, h, "demo", "7", "/demo/app.css?v=2")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/demo/app.css?v=2" {
		t.Fatalf("unlock: %d Location=%q", rr.Code, rr.Header().Get("Location"))
	}
	c := siteCookie(t, rr)
	if c.Path != "/demo/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie attrs: path=%q httponly=%v samesite=%v", c.Path, c.HttpOnly, c.SameSite)
	}

	page := getWith(h, "/demo/", c)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "SECRET-demo") {
		t.Fatalf("unlocked page: %d %s", page.Code, page.Body.String())
	}
	if cc := page.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Fatalf("protected Cache-Control = %q", cc)
	}
	if css := getWith(h, "/demo/app.css", c); css.Code != http.StatusOK || css.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("unlocked asset: %d cc=%q", css.Code, css.Header().Get("Cache-Control"))
	}
}

func TestUnlockCookieIsPerSiteAndPerPassword(t *testing.T) {
	srv, h := newTestServer(t)
	demoID := protectSite(t, srv, "demo", "a")
	protectSite(t, srv, "other", "a") // same password, different site

	c := siteCookie(t, unlock(srv, h, "demo", "a", "/demo/"))

	// A demo cookie replayed against another site does not open it.
	c2 := *c
	c2.Path = "/other/"
	if rr := getWith(h, "/other/", &c2); rr.Code != http.StatusUnauthorized {
		t.Fatalf("cross-site cookie: status = %d, want 401", rr.Code)
	}

	// Changing the password invalidates earlier unlocks.
	if err := srv.setSitePassword(context.Background(), demoID, "b"); err != nil {
		t.Fatal(err)
	}
	if rr := getWith(h, "/demo/", c); rr.Code != http.StatusUnauthorized {
		t.Fatalf("after password change: status = %d, want 401", rr.Code)
	}

	// A tampered cookie is rejected.
	bad := &http.Cookie{Name: auth.SiteCookieName, Value: c.Value + "x"}
	if rr := getWith(h, "/demo/", bad); rr.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie: status = %d, want 401", rr.Code)
	}
}

func TestAdminSessionBypassesSitePassword(t *testing.T) {
	srv, h := newTestServer(t)
	protectSite(t, srv, "demo", "x")

	rr := getWith(h, "/demo/", srv.auth.IssueSessionCookie())
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "SECRET-demo") {
		t.Fatalf("admin view: %d %s", rr.Code, rr.Body.String())
	}
	// An expired/forged admin session does not.
	forged := &http.Cookie{Name: auth.SessionCookieName, Value: "nope"}
	if rr := getWith(h, "/demo/", forged); rr.Code != http.StatusUnauthorized {
		t.Fatalf("forged session: status = %d, want 401", rr.Code)
	}
}

func TestRemovingPasswordMakesSitePublic(t *testing.T) {
	srv, h := newTestServer(t)
	id := protectSite(t, srv, "demo", "x")
	if err := srv.setSitePassword(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	rr := get(h, "/demo/")
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("public again: %d cc=%q", rr.Code, rr.Header().Get("Cache-Control"))
	}
}

func TestUnlockEndpointGuards(t *testing.T) {
	srv, h := newTestServer(t)
	protectSite(t, srv, "demo", "x")
	seedProject(t, srv, "gone", model.StatusUnlinked)

	if rr := unlock(srv, h, "nope", "x", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown slug: %d", rr.Code)
	}
	if rr := unlock(srv, h, "gone", "x", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unlinked slug: %d", rr.Code)
	}

	// Missing CSRF token.
	req := httptest.NewRequest(http.MethodPost, "/_app/unlock/demo", strings.NewReader("password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing csrf: %d", rr.Code)
	}

	// Open-redirect attempts fall back to the site root.
	for _, next := range []string{"//evil.example/", "https://evil.example/", "/other/", "/demo", "", "/demo/\r\nX: y"} {
		rr := unlock(srv, h, "demo", "x", next)
		if loc := rr.Header().Get("Location"); rr.Code != http.StatusSeeOther || loc != "/demo/" {
			t.Fatalf("next=%q: %d Location=%q", next, rr.Code, loc)
		}
	}
}

func TestValidateSitePassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"":                      false,
		"1":                     true,
		" ":                     true,
		strings.Repeat("a", 72): true,
		strings.Repeat("a", 73): false,
	} {
		if err := validateSitePassword(pw); (err == nil) != ok {
			t.Errorf("validateSitePassword(len %d) = %v, want ok=%v", len(pw), err, ok)
		}
	}
}

func TestAdminSetAndRemoveSitePassword(t *testing.T) {
	srv, h := newTestServer(t)
	id := protectSite(t, srv, "demo", "")

	rr := authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/password", url.Values{"password": {""}})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "at least 1 character") {
		t.Fatalf("empty password: %d", rr.Code)
	}

	rr = authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/password", url.Values{"password": {"9"}})
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "flash=Password+set.") {
		t.Fatalf("set: %d Location=%q", rr.Code, rr.Header().Get("Location"))
	}
	if rr := get(h, "/demo/"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("after set: %d, want 401", rr.Code)
	}
	if rr := authedGet(srv, h, "/_app/projects/"+id); rr.Code != http.StatusOK {
		t.Fatalf("edit page: %d", rr.Code)
	}
	if rr := authedGet(srv, h, "/_app/projects"); rr.Code != http.StatusOK {
		t.Fatalf("list page: %d", rr.Code)
	}

	rr = authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/password", url.Values{"password": {"10"}})
	if !strings.Contains(rr.Header().Get("Location"), "flash=Password+changed.") {
		t.Fatalf("change: Location=%q", rr.Header().Get("Location"))
	}

	rr = authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/password/remove", url.Values{})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d", rr.Code)
	}
	if rr := get(h, "/demo/"); rr.Code != http.StatusOK {
		t.Fatalf("after remove: %d, want 200", rr.Code)
	}

	// Unauthenticated callers cannot change it.
	req := httptest.NewRequest(http.MethodPost, "/_app/projects/"+id+"/password", strings.NewReader("password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != loginPath {
		t.Fatalf("unauthenticated: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAPISitePassword(t *testing.T) {
	srv, h := newTestServer(t)
	tok := seedAPIKey(t, srv)

	rr := apiReq(t, h, tok, http.MethodPost, "/api/v1/projects", map[string]any{"name": "P", "slug": "p", "password": "1"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var p apiProject
	decodeBody(t, rr, &p)
	if !p.PasswordProtected || strings.Contains(rr.Body.String(), "$2") {
		t.Fatalf("create response: %s", rr.Body.String())
	}
	if rr := get(h, "/p/"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("api-protected site: %d", rr.Code)
	}

	long := strings.Repeat("a", 73)
	rr = apiReq(t, h, tok, http.MethodPatch, "/api/v1/projects/"+p.ID, map[string]any{"name": "Renamed", "password": long})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too long: %d %s", rr.Code, rr.Body.String())
	}
	// Validation failed before any write: the name is unchanged.
	if got, _ := srv.store.GetProject(context.Background(), p.ID); got.Name != "P" {
		t.Fatalf("partial write: name = %q", got.Name)
	}

	rr = apiReq(t, h, tok, http.MethodPatch, "/api/v1/projects/"+p.ID, map[string]any{"password": ""})
	decodeBody(t, rr, &p)
	if rr.Code != http.StatusOK || p.PasswordProtected {
		t.Fatalf("remove: %d %s", rr.Code, rr.Body.String())
	}
	if rr := get(h, "/p/"); rr.Code != http.StatusOK {
		t.Fatalf("public again: %d", rr.Code)
	}

	rr = apiReq(t, h, tok, http.MethodPost, "/api/v1/projects", map[string]any{"name": "Q", "slug": "q", "password": long})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create too long: %d", rr.Code)
	}
	if _, err := srv.store.GetProjectBySlug(context.Background(), "q"); err == nil {
		t.Fatal("project created despite invalid password")
	}
}

func TestMCPSitePassword(t *testing.T) {
	srv, h := newTestServer(t)
	tok := seedAPIKey(t, srv)
	protectSite(t, srv, "demo", "")

	isErr, out, raw := mcpTool(t, h, tok, "update_project", map[string]any{"project": "demo", "password": "z"})
	if isErr || out["password_protected"] != true {
		t.Fatalf("set via mcp: %v %s", isErr, raw)
	}
	if rr := get(h, "/demo/"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("after mcp set: %d", rr.Code)
	}
	isErr, out, raw = mcpTool(t, h, tok, "update_project", map[string]any{"project": "demo", "password": ""})
	if isErr || out["password_protected"] != false {
		t.Fatalf("remove via mcp: %v %s", isErr, raw)
	}
	isErr, out, raw = mcpTool(t, h, tok, "create_project", map[string]any{"name": "N", "slug": "n", "password": "k"})
	if isErr || out["password_protected"] != true {
		t.Fatalf("create via mcp: %v %s", isErr, raw)
	}
}
