package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/model"
)

// Template helpers used by the file manager and page headers.
func TestTemplateFuncs(t *testing.T) {
	dir := funcMap["pathDir"].(func(string) string)
	base := funcMap["pathBase"].(func(string) string)
	plural := funcMap["plural"].(func(int, string, string) string)
	for _, c := range []struct{ in, dir, base string }{
		{"index.html", "", "index.html"},
		{"assets/css/site.css", "assets/css/", "site.css"},
		{"a/b", "a/", "b"},
	} {
		if got := dir(c.in); got != c.dir {
			t.Errorf("pathDir(%q) = %q, want %q", c.in, got, c.dir)
		}
		if got := base(c.in); got != c.base {
			t.Errorf("pathBase(%q) = %q, want %q", c.in, got, c.base)
		}
	}
	if plural(1, "file", "files") != "file" || plural(0, "file", "files") != "files" || plural(2, "file", "files") != "files" {
		t.Error("plural picks the wrong form")
	}
}

// A failed upload during create must surface as an error, not a success flash.
func TestCreateProjectUploadFailureIsError(t *testing.T) {
	srv, h := newTestServer(t)
	zipData := makeZip(t, map[string]string{"../evil.html": "x"})
	rr := authedUpload(srv, h, "/_app/projects",
		map[string]string{"name": "Bad zip", "slug": "bad-zip"},
		[]uploadFile{{name: "site.zip", data: zipData}})
	loc := rr.Header().Get("Location")
	if rr.Code != http.StatusSeeOther || !strings.Contains(loc, "error=") || strings.Contains(loc, "flash=") {
		t.Fatalf("create with unsafe zip: %d Location=%q, want an error= redirect", rr.Code, loc)
	}
	q, _ := url.ParseQuery(loc[strings.Index(loc, "?")+1:])
	if !strings.HasPrefix(q.Get("error"), "Project created, but the files were not uploaded") {
		t.Fatalf("error message = %q", q.Get("error"))
	}
}

// Unlink and relink confirm what happened, naming the project.
func TestUnlinkRelinkFlash(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	rr := authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/unlink", url.Values{})
	if loc := rr.Header().Get("Location"); !strings.HasPrefix(loc, "/_app/projects?flash=") || !strings.Contains(loc, "offline") {
		t.Fatalf("unlink Location = %q", loc)
	}
	rr = authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/relink", url.Values{})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "live+again") {
		t.Fatalf("relink Location = %q", loc)
	}
	// The list renders the flash.
	if body := authedGet(srv, h, rr.Header().Get("Location")).Body.String(); !strings.Contains(body, "is live again.") {
		t.Fatal("relink flash not rendered on the project list")
	}
}

// Revoking a token that no longer exists reports an error in the tokens card.
func TestRevokeMissingTokenIsError(t *testing.T) {
	srv, h := newTestServer(t)
	rr := authedForm(srv, h, http.MethodPost, "/_app/settings/api-keys/nope/delete", url.Values{})
	loc := rr.Header().Get("Location")
	if rr.Code != http.StatusSeeOther || !strings.Contains(loc, "error=") {
		t.Fatalf("revoke missing: %d Location=%q", rr.Code, loc)
	}
	body := authedGet(srv, h, loc).Body.String()
	if !strings.Contains(body, "That token no longer exists.") || !strings.Contains(body, `role="alert"`) {
		t.Fatal("missing-token error not rendered as an alert")
	}
}

// The file manager warns when the landing page file is missing (the site root
// would 404), and stays quiet when it exists.
func TestFilesWarnsMissingLandingPage(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	if err := srv.fs.WriteFile(id, "style.css", []byte("body{}")); err != nil {
		t.Fatal(err)
	}
	body := authedGet(srv, h, "/_app/projects/"+id+"/files").Body.String()
	if !strings.Contains(body, "No landing page.") {
		t.Fatal("missing landing page not flagged")
	}
	if err := srv.fs.WriteFile(id, "index.html", []byte("<p>hi</p>")); err != nil {
		t.Fatal(err)
	}
	body = authedGet(srv, h, "/_app/projects/"+id+"/files").Body.String()
	if strings.Contains(body, "No landing page.") {
		t.Fatal("warning shown although index.html exists")
	}
	// Nested paths render folder and name separately; the rename form
	// prefills only the base name.
	if err := srv.fs.WriteFile(id, "assets/css/site.css", []byte("x")); err != nil {
		t.Fatal(err)
	}
	body = authedGet(srv, h, "/_app/projects/"+id+"/files/rename?path=assets/css/site.css").Body.String()
	if !strings.Contains(body, `<span class="path__dir">assets/css/</span><span class="path__base">site.css</span>`) {
		t.Fatal("nested path not split into folder + name")
	}
	if !strings.Contains(body, `name="name" value="site.css"`) {
		t.Fatal("rename form should prefill the base name only")
	}
}
