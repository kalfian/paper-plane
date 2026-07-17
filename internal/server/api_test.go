package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/apikey"
	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/store"
)

// seedAPIKey creates an API key row and returns the plaintext token to present.
func seedAPIKey(t *testing.T, srv *Server) string {
	t.Helper()
	token, hash := apikey.Generate()
	k := &model.APIKey{ID: store.NewID(), Name: "test", KeyHash: hash}
	if err := srv.store.CreateAPIKey(context.Background(), k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return token
}

// apiReq issues an authenticated JSON request against the handler and returns
// the recorder. body may be nil.
func apiReq(t *testing.T, h http.Handler, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
}

// --- auth ---

func TestAPIRequiresKey(t *testing.T) {
	_, h := newTestServer(t)
	for _, tc := range []struct{ name, auth string }{
		{"no header", ""},
		{"wrong scheme", "Basic abc"},
		{"unknown key", "Bearer pp_deadbeef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
			var body apiErrorBody
			decodeBody(t, rr, &body)
			if body.Error.Code != "unauthorized" {
				t.Fatalf("error code = %q, want unauthorized", body.Error.Code)
			}
		})
	}
}

func TestAPIIndexIsPublic(t *testing.T) {
	_, h := newTestServer(t)
	for _, p := range []string{"/api/v1", "/api/v1/"} {
		rr := get(h, p)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", p, rr.Code)
		}
	}
}

func TestAPIUnknownPathIs404(t *testing.T) {
	_, h := newTestServer(t)
	// An unmatched path under /api/v1/ must be a JSON 404, not the discovery doc.
	rr := get(h, "/api/v1/bogus")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rr.Code, rr.Body.String())
	}
	var body apiErrorBody
	decodeBody(t, rr, &body)
	if body.Error.Code != "not_found" {
		t.Fatalf("error code = %q, want not_found", body.Error.Code)
	}
}

func TestAPIKeyTouchedOnUse(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	rr := apiReq(t, h, token, http.MethodGet, "/api/v1/projects", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	keys, err := srv.store.ListAPIKeys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListAPIKeys: %v (n=%d)", err, len(keys))
	}
	if keys[0].LastUsedAt == nil {
		t.Fatal("LastUsedAt not recorded after API use")
	}
}

// --- projects ---

func TestAPIProjectLifecycle(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)

	// Create.
	rr := apiReq(t, h, token, http.MethodPost, "/api/v1/projects",
		map[string]string{"name": "Docs", "slug": "docs"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	var created apiProject
	decodeBody(t, rr, &created)
	if created.Slug != "docs" || created.Status != "active" || created.ID == "" {
		t.Fatalf("created project unexpected: %+v", created)
	}
	// Placeholder index makes the site non-empty.
	if created.FileCount != 1 {
		t.Fatalf("created file_count = %d, want 1", created.FileCount)
	}
	id := created.ID

	// Get.
	rr = apiReq(t, h, token, http.MethodGet, "/api/v1/projects/"+id, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rr.Code)
	}

	// List.
	rr = apiReq(t, h, token, http.MethodGet, "/api/v1/projects", nil)
	var list struct {
		Projects []apiProject `json:"projects"`
	}
	decodeBody(t, rr, &list)
	if len(list.Projects) != 1 {
		t.Fatalf("list len = %d, want 1", len(list.Projects))
	}

	// Update name + status.
	rr = apiReq(t, h, token, http.MethodPatch, "/api/v1/projects/"+id,
		map[string]any{"name": "Documentation", "status": "unlinked"})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var updated apiProject
	decodeBody(t, rr, &updated)
	if updated.Name != "Documentation" || updated.Status != "unlinked" {
		t.Fatalf("updated project unexpected: %+v", updated)
	}

	// Delete.
	rr = apiReq(t, h, token, http.MethodDelete, "/api/v1/projects/"+id, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rr.Code)
	}
	rr = apiReq(t, h, token, http.MethodGet, "/api/v1/projects/"+id, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", rr.Code)
	}
}

func TestAPIProjectCreateValidation(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)

	cases := []struct {
		name string
		body map[string]string
		want int
	}{
		{"missing name", map[string]string{"slug": "ok"}, http.StatusUnprocessableEntity},
		{"missing slug", map[string]string{"name": "ok"}, http.StatusUnprocessableEntity},
		{"bad slug", map[string]string{"name": "ok", "slug": "Bad Slug"}, http.StatusUnprocessableEntity},
		{"reserved slug", map[string]string{"name": "ok", "slug": "api"}, http.StatusUnprocessableEntity},
		{"bad status", map[string]string{"name": "ok", "slug": "ok", "status": "weird"}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := apiReq(t, h, token, http.MethodPost, "/api/v1/projects", tc.body)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestAPIProjectCreateDuplicateSlug(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	seedProject(t, srv, "taken", model.StatusActive)

	rr := apiReq(t, h, token, http.MethodPost, "/api/v1/projects",
		map[string]string{"name": "x", "slug": "taken"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	var body apiErrorBody
	decodeBody(t, rr, &body)
	if body.Error.Code != "slug_exists" {
		t.Fatalf("error code = %q, want slug_exists", body.Error.Code)
	}
}

func TestAPIProjectSetIndexFile(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	id := seedProject(t, srv, "site", model.StatusActive)

	// Setting a non-existent index file is rejected.
	rr := apiReq(t, h, token, http.MethodPatch, "/api/v1/projects/"+id,
		map[string]any{"index_file": "nope.html"})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}

	// Write a file, then set it as the index.
	if err := srv.fs.WriteFile(id, "home.html", []byte("<h1>hi</h1>")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rr = apiReq(t, h, token, http.MethodPatch, "/api/v1/projects/"+id,
		map[string]any{"index_file": "home.html"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var p apiProject
	decodeBody(t, rr, &p)
	if p.IndexFile != "home.html" {
		t.Fatalf("index_file = %q, want home.html", p.IndexFile)
	}
}

// --- files ---

func TestAPIFileLifecycle(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	id := seedProject(t, srv, "site", model.StatusActive)

	// Create a text file (201).
	rr := apiReq(t, h, token, http.MethodPut, "/api/v1/projects/"+id+"/files/index.html",
		map[string]string{"content": "<h1>Hello</h1>"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("put status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	// Overwrite (200).
	rr = apiReq(t, h, token, http.MethodPut, "/api/v1/projects/"+id+"/files/index.html",
		map[string]string{"content": "<h1>Hi</h1>"})
	if rr.Code != http.StatusOK {
		t.Fatalf("overwrite status = %d, want 200", rr.Code)
	}

	// Read back as utf8.
	rr = apiReq(t, h, token, http.MethodGet, "/api/v1/projects/"+id+"/files/index.html", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rr.Code)
	}
	var fc apiFileContent
	decodeBody(t, rr, &fc)
	if fc.Encoding != "utf8" || fc.Content != "<h1>Hi</h1>" {
		t.Fatalf("read back unexpected: %+v", fc)
	}

	// Nested path is created.
	rr = apiReq(t, h, token, http.MethodPut, "/api/v1/projects/"+id+"/files/css/app.css",
		map[string]string{"content": "body{}"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("nested put status = %d, want 201", rr.Code)
	}

	// List shows both.
	rr = apiReq(t, h, token, http.MethodGet, "/api/v1/projects/"+id+"/files", nil)
	var fl struct {
		Files []apiFile `json:"files"`
	}
	decodeBody(t, rr, &fl)
	if len(fl.Files) != 2 {
		t.Fatalf("file list len = %d, want 2 (%+v)", len(fl.Files), fl.Files)
	}

	// Delete.
	rr = apiReq(t, h, token, http.MethodDelete, "/api/v1/projects/"+id+"/files/css/app.css", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rr.Code)
	}
	rr = apiReq(t, h, token, http.MethodGet, "/api/v1/projects/"+id+"/files/css/app.css", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", rr.Code)
	}
}

func TestAPIFileBase64RoundTrip(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	id := seedProject(t, srv, "site", model.StatusActive)

	// A PNG header is not valid UTF-8, so it must round-trip via base64.
	binary := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe}
	rr := apiReq(t, h, token, http.MethodPut, "/api/v1/projects/"+id+"/files/logo.png",
		map[string]string{"content": base64.StdEncoding.EncodeToString(binary), "encoding": "base64"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("put status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	rr = apiReq(t, h, token, http.MethodGet, "/api/v1/projects/"+id+"/files/logo.png", nil)
	var fc apiFileContent
	decodeBody(t, rr, &fc)
	if fc.Encoding != "base64" {
		t.Fatalf("encoding = %q, want base64", fc.Encoding)
	}
	got, err := base64.StdEncoding.DecodeString(fc.Content)
	if err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("round-trip mismatch: got %x, want %x", got, binary)
	}
}

func TestAPIFilePathTraversalRejected(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	id := seedProject(t, srv, "site", model.StatusActive)

	rr := apiReq(t, h, token, http.MethodPut, "/api/v1/projects/"+id+"/files/../escape.txt",
		map[string]string{"content": "x"})
	// The traversal is cleaned by the router/sitefs; it must never write outside
	// the site. Accept either a 400 unsafe_path or a 404 (path resolved away),
	// but never a 2xx.
	if rr.Code >= 200 && rr.Code < 300 {
		t.Fatalf("traversal write unexpectedly succeeded: %d (%s)", rr.Code, rr.Body.String())
	}
}

func TestAPIFileBadEncoding(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	id := seedProject(t, srv, "site", model.StatusActive)

	rr := apiReq(t, h, token, http.MethodPut, "/api/v1/projects/"+id+"/files/x.txt",
		map[string]string{"content": "!!!not base64!!!", "encoding": "base64"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestAPIProjectNotFound(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	rr := apiReq(t, h, token, http.MethodGet, "/api/v1/projects/nonexistent/files", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestAPIRejectsUnknownJSONFields(t *testing.T) {
	srv, h := newTestServer(t)
	token := seedAPIKey(t, srv)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects",
		strings.NewReader(`{"name":"x","slug":"y","bogus":true}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
