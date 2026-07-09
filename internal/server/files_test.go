package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/model"
)

// --- helpers ---

func authedForm(srv *Server, h http.Handler, method, path string, form url.Values) *httptest.ResponseRecorder {
	form.Set("csrf_token", srv.auth.IssueCSRFToken())
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(srv.auth.IssueSessionCookie())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func authedGet(srv *Server, h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(srv.auth.IssueSessionCookie())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

type uploadFile struct {
	name string
	data []byte
}

func authedUpload(srv *Server, h http.Handler, path string, fields map[string]string, files []uploadFile) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("csrf_token", srv.auth.IssueCSRFToken())
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for _, f := range files {
		fw, _ := mw.CreateFormFile("files", f.name)
		_, _ = fw.Write(f.data)
	}
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(srv.auth.IssueSessionCookie())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func makeZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// --- slug validation ---

func TestValidateSlug(t *testing.T) {
	cases := []struct {
		slug string
		ok   bool
	}{
		{"demo", true},
		{"my-site-2", true},
		{"a", true},
		{"", false},
		{"-lead", false},
		{"UPPER", false},
		{"has_underscore", false},
		{"has space", false},
		{"_app", false},
		{"healthz", false},
		{strings.Repeat("a", 64), false},
		{strings.Repeat("a", 63), true},
	}
	for _, tc := range cases {
		got := validateSlug(tc.slug) == ""
		if got != tc.ok {
			t.Errorf("validateSlug(%q) valid=%v, want %v", tc.slug, got, tc.ok)
		}
	}
}

// --- project create ---

func TestCreateProjectPlaceholder(t *testing.T) {
	srv, h := newTestServer(t)
	rr := authedUpload(srv, h, "/_app/projects", map[string]string{"name": "Demo", "slug": "demo"}, nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303; body=%s", rr.Code, rr.Body.String())
	}
	p, err := srv.store.GetProjectBySlug(context.Background(), "demo")
	if err != nil {
		t.Fatalf("project not created: %v", err)
	}
	data, err := srv.fs.ReadFile(p.ID, "index.html")
	if err != nil {
		t.Fatalf("placeholder not written: %v", err)
	}
	if !bytes.Contains(data, []byte("static site kosong")) {
		t.Fatalf("placeholder content unexpected: %s", data)
	}
}

func TestCreateProjectDuplicateSlug(t *testing.T) {
	srv, h := newTestServer(t)
	seedProject(t, srv, "demo", model.StatusActive)

	rr := authedUpload(srv, h, "/_app/projects", map[string]string{"name": "Demo2", "slug": "demo"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("dup slug status = %d, want 200 (re-render)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "already in use") {
		t.Fatalf("dup slug missing error message: %s", rr.Body.String())
	}
}

func TestCreateProjectInvalidSlug(t *testing.T) {
	srv, h := newTestServer(t)
	rr := authedUpload(srv, h, "/_app/projects", map[string]string{"name": "X", "slug": "Bad Slug"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("invalid slug status = %d, want 200", rr.Code)
	}
	if _, err := srv.store.GetProjectBySlug(context.Background(), "Bad Slug"); err == nil {
		t.Fatal("invalid slug should not have created a project")
	}
}

func TestCreateProjectWithZip(t *testing.T) {
	srv, h := newTestServer(t)
	zipData := makeZip(t, map[string]string{
		"index.html": "<head></head>root",
		"js/app.js":  "console.log(1)",
	})
	rr := authedUpload(srv, h, "/_app/projects",
		map[string]string{"name": "Zipped", "slug": "zipped"},
		[]uploadFile{{name: "site.zip", data: zipData}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create-with-zip status = %d; body=%s", rr.Code, rr.Body.String())
	}
	p, err := srv.store.GetProjectBySlug(context.Background(), "zipped")
	if err != nil {
		t.Fatalf("project not created: %v", err)
	}
	if _, err := srv.fs.ReadFile(p.ID, "js/app.js"); err != nil {
		t.Fatalf("zip content not extracted: %v", err)
	}
}

// --- unlink / relink / delete ---

func TestUnlinkRelink(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)

	if rr := authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/unlink", url.Values{}); rr.Code != http.StatusSeeOther {
		t.Fatalf("unlink status = %d", rr.Code)
	}
	p, _ := srv.store.GetProject(context.Background(), id)
	if p.Status != model.StatusUnlinked {
		t.Fatalf("status = %q, want unlinked", p.Status)
	}

	if rr := authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/relink", url.Values{}); rr.Code != http.StatusSeeOther {
		t.Fatalf("relink status = %d", rr.Code)
	}
	p, _ = srv.store.GetProject(context.Background(), id)
	if p.Status != model.StatusActive {
		t.Fatalf("status = %q, want active", p.Status)
	}
}

func TestDeleteProjectRemovesSite(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	if err := srv.fs.WriteFile(id, "index.html", []byte("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rr := authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/delete", url.Values{})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d", rr.Code)
	}
	if _, err := srv.store.GetProject(context.Background(), id); err == nil {
		t.Fatal("project row still present after delete")
	}
	if _, err := srv.fs.Stat(id, "index.html"); !isNotExist(err) {
		t.Fatalf("site file still present after delete: %v", err)
	}
}

// --- file edit / save / delete ---

func TestFileEditRejectsNonText(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	if err := srv.fs.WriteFile(id, "logo.png", []byte{0x89, 0x50}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rr := authedGet(srv, h, "/_app/projects/"+id+"/files/edit?path=logo.png")
	if rr.Code != http.StatusOK {
		t.Fatalf("edit-nontext status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "cannot be edited") {
		t.Fatalf("expected non-editable message: %s", rr.Body.String())
	}
}

func TestFileEditAndSaveText(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	if err := srv.fs.WriteFile(id, "page.html", []byte("<p>old</p>")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rr := authedGet(srv, h, "/_app/projects/"+id+"/files/edit?path=page.html")
	// html/template escapes the content inside the textarea.
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "&lt;p&gt;old&lt;/p&gt;") {
		t.Fatalf("edit did not show content: %d %q", rr.Code, rr.Body.String())
	}

	rr = authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/files/save",
		url.Values{"path": {"page.html"}, "content": {"<p>new</p>"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d", rr.Code)
	}
	got, _ := srv.fs.ReadFile(id, "page.html")
	if string(got) != "<p>new</p>" {
		t.Fatalf("saved content = %q", got)
	}

	// Save redirects back to the editor for the same file (not the list) so the
	// editor stays open with a success flash.
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/files/edit") || !strings.Contains(loc, "path=page.html") {
		t.Fatalf("save should redirect to the editor for the same file; Location = %q", loc)
	}
	rr = authedGet(srv, h, loc)
	if rr.Code != http.StatusOK {
		t.Fatalf("post-save editor status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "&lt;p&gt;new&lt;/p&gt;") {
		t.Fatalf("post-save editor did not reload the file contents: %q", body)
	}
	if !strings.Contains(body, "File saved.") {
		t.Fatalf("post-save editor missing success flash: %q", body)
	}
}

func TestFileSaveRejectsNonText(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	rr := authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/files/save",
		url.Values{"path": {"evil.php"}, "content": {"<?php ?>"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save-nontext status = %d, want 303 with error", rr.Code)
	}
	if _, err := srv.fs.Stat(id, "evil.php"); !isNotExist(err) {
		t.Fatal("non-text file should not have been written")
	}
}

func TestFileSaveRejectsTraversal(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	rr := authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/files/save",
		url.Values{"path": {"../escape.html"}, "content": {"x"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("traversal save status = %d, want 400", rr.Code)
	}
}

func TestFileUploadAndDelete(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)

	rr := authedUpload(srv, h, "/_app/projects/"+id+"/files", nil,
		[]uploadFile{{name: "notes.txt", data: []byte("hi")}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d", rr.Code)
	}
	if got, err := srv.fs.ReadFile(id, "notes.txt"); err != nil || string(got) != "hi" {
		t.Fatalf("uploaded file = %q, %v", got, err)
	}

	rr = authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/files/delete",
		url.Values{"path": {"notes.txt"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d", rr.Code)
	}
	if _, err := srv.fs.Stat(id, "notes.txt"); !isNotExist(err) {
		t.Fatal("file still present after delete")
	}
}

func TestFileDeleteRejectsTraversal(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	rr := authedForm(srv, h, http.MethodPost, "/_app/projects/"+id+"/files/delete",
		url.Values{"path": {"../../etc/passwd"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("traversal delete status = %d, want 400", rr.Code)
	}
}

func TestUploadStripsClientPath(t *testing.T) {
	srv, h := newTestServer(t)
	id := seedProject(t, srv, "demo", model.StatusActive)
	// A malicious client filename must be reduced to its base name.
	authedUpload(srv, h, "/_app/projects/"+id+"/files", nil,
		[]uploadFile{{name: "../../evil.txt", data: []byte("x")}})
	if _, err := srv.fs.Stat(id, "evil.txt"); err != nil {
		t.Fatalf("expected file stored at base name: %v", err)
	}
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
