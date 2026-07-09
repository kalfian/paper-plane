package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/store"
)

// seedProject creates a project row and site directory, returning its ID.
func seedProject(t *testing.T, srv *Server, slug string, status model.Status) string {
	t.Helper()
	p := &model.Project{ID: store.NewID(), Name: slug, Slug: slug, Status: status}
	if err := srv.store.CreateProject(context.Background(), p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := srv.fs.CreateSite(p.ID); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	return p.ID
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func TestServeIndexWithBaseInjection(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	if err := srv.fs.WriteFile(id, "index.html", []byte("<html><head></head><body>hi</body></html>")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rr := get(h, "/demo/")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `<base href="/demo/">`) {
		t.Fatalf("index missing injected base: %s", rr.Body.String())
	}
	// Injected right after <head>.
	if !strings.Contains(rr.Body.String(), `<head><base href="/demo/">`) {
		t.Fatalf("base not placed right after head: %s", rr.Body.String())
	}
}

func TestServeIndexBaseNotDuplicated(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	body := `<html><head><base href="/custom/"></head><body>hi</body></html>`
	if err := srv.fs.WriteFile(id, "index.html", []byte(body)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rr := get(h, "/demo/")
	got := rr.Body.String()
	if strings.Count(got, "<base") != 1 {
		t.Fatalf("expected exactly one <base>, got: %s", got)
	}
	if !strings.Contains(got, `<base href="/custom/">`) {
		t.Fatalf("existing base was altered: %s", got)
	}
}

func TestServeStaticFile(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	if err := srv.fs.WriteFile(id, "css/app.css", []byte("body{color:red}")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rr := get(h, "/demo/css/app.css")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "body{color:red}" {
		t.Fatalf("body = %q", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css", ct)
	}
	// Non-HTML assets should NOT be rewritten with a base tag.
	if strings.Contains(rr.Body.String(), "<base") {
		t.Fatal("static asset was rewritten")
	}
}

func TestServeDefaultIndexForBareSlug(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	if err := srv.fs.WriteFile(id, "index.html", []byte("<head></head>index")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rr := get(h, "/demo/")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "index") {
		t.Fatalf("bare slug did not serve index: %d %q", rr.Code, rr.Body.String())
	}
}

func TestServeRedirectTrailingSlash(t *testing.T) {
	srv, h := newTestServer(t)
	seedProject(t, srv, "demo", model.StatusActive)

	rr := get(h, "/demo")
	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/demo/" {
		t.Fatalf("Location = %q, want /demo/", loc)
	}
}

func TestServeUnlinked404(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusUnlinked)
	if err := srv.fs.WriteFile(id, "index.html", []byte("<head></head>x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rr := get(h, "/demo/")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unlinked status = %d, want 404", rr.Code)
	}
}

func TestServeUnknownSlug404(t *testing.T) {
	_, h := newTestServer(t)
	rr := get(h, "/nope/")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown slug status = %d, want 404", rr.Code)
	}
}

func TestServeMissingFile404(t *testing.T) {
	srv, h := newTestServer(t)
	seedProject(t, srv, "demo", model.StatusActive)
	rr := get(h, "/demo/missing.txt")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing file status = %d, want 404", rr.Code)
	}
}

func TestAdminPathsNotServedAsSite(t *testing.T) {
	_, h := newTestServer(t)
	// Unauthenticated /_app/projects must redirect to login, never fall through
	// to the slug resolver.
	rr := get(h, "/_app/projects")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("/_app/projects status = %d, want 303 (login redirect)", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != loginPath {
		t.Fatalf("Location = %q, want %q", loc, loginPath)
	}
}
