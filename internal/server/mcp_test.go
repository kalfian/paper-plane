package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/apikey"
	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/store"
)

// seedScopedKey creates an API key with the given scope and returns its token.
func seedScopedKey(t *testing.T, srv *Server, scope model.KeyScope) string {
	t.Helper()
	token, hash := apikey.Generate()
	k := &model.APIKey{ID: store.NewID(), Name: "t-" + string(scope), KeyHash: hash, Scope: scope}
	if err := srv.store.CreateAPIKey(context.Background(), k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return token
}

// mcpPost sends one raw JSON-RPC message to /api/mcp.
func mcpPost(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, mcpPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// mcpTool calls a tool and returns (isError, decoded JSON text payload, raw text).
func mcpTool(t *testing.T, h http.Handler, token, name string, args any) (bool, map[string]any, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	rr := mcpPost(t, h, token, string(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", name, rr.Code, rr.Body.String())
	}
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: decode %s: %v", name, rr.Body.String(), err)
	}
	if resp.Error != nil {
		t.Fatalf("%s: protocol error %s", name, resp.Error.Message)
	}
	text := ""
	if len(resp.Result.Content) > 0 {
		text = resp.Result.Content[0].Text
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(text), &payload)
	return resp.Result.IsError, payload, text
}

func mustTool(t *testing.T, h http.Handler, token, name string, args any) map[string]any {
	t.Helper()
	isErr, payload, text := mcpTool(t, h, token, name, args)
	if isErr {
		t.Fatalf("%s returned error: %s", name, text)
	}
	return payload
}

func TestMCPAuthAndScopes(t *testing.T) {
	srv, h := newTestServer(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	if rr := mcpPost(t, h, "", ping); rr.Code != http.StatusUnauthorized || rr.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("no token: %d", rr.Code)
	}
	if rr := mcpPost(t, h, "pp_nope", ping); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", rr.Code)
	}

	apiOnly := seedScopedKey(t, srv, model.ScopeAPI)
	mcpOnly := seedScopedKey(t, srv, model.ScopeMCP)
	all := seedScopedKey(t, srv, model.ScopeAll)

	if rr := mcpPost(t, h, apiOnly, ping); rr.Code != http.StatusForbidden {
		t.Fatalf("api-only key on MCP: %d, want 403", rr.Code)
	}
	for _, tok := range []string{mcpOnly, all} {
		if rr := mcpPost(t, h, tok, ping); rr.Code != http.StatusOK {
			t.Fatalf("MCP-capable key on MCP: %d %s", rr.Code, rr.Body.String())
		}
	}
	// And the REST API enforces the inverse.
	if rr := apiReq(t, h, mcpOnly, http.MethodGet, "/api/v1/projects", nil); rr.Code != http.StatusForbidden {
		t.Fatalf("mcp-only key on REST: %d, want 403", rr.Code)
	}
	for _, tok := range []string{apiOnly, all} {
		if rr := apiReq(t, h, tok, http.MethodGet, "/api/v1/projects", nil); rr.Code != http.StatusOK {
			t.Fatalf("API-capable key on REST: %d", rr.Code)
		}
	}

	// GET is answered (after auth) with 405: no SSE stream is offered.
	r := httptest.NewRequest(http.MethodGet, mcpPath, nil)
	r.Header.Set("Authorization", "Bearer "+mcpOnly)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/mcp: %d, want 405", rr.Code)
	}
}

func TestMCPInitializeAndToolList(t *testing.T) {
	srv, h := newTestServer(t)
	tok := seedScopedKey(t, srv, model.ScopeMCP)

	rr := mcpPost(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"protocolVersion":"2025-06-18"`) || !strings.Contains(rr.Body.String(), `"paper-plane"`) {
		t.Fatalf("initialize: %d %s", rr.Code, rr.Body.String())
	}
	if rr := mcpPost(t, h, tok, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); rr.Code != http.StatusAccepted {
		t.Fatalf("initialized notification: %d", rr.Code)
	}

	rr = mcpPost(t, h, tok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range resp.Result.Tools {
		var sch map[string]any
		if err := json.Unmarshal(tool.InputSchema, &sch); err != nil || sch["type"] != "object" {
			t.Errorf("tool %s has invalid inputSchema: %s", tool.Name, tool.InputSchema)
		}
		got[tool.Name] = true
	}
	for _, want := range []string{"list_projects", "get_project", "create_project", "update_project", "delete_project",
		"list_files", "read_file", "write_file", "upload_files", "upload_zip", "rename_file", "delete_file"} {
		if !got[want] {
			t.Errorf("tool %s missing", want)
		}
	}
}

// TestMCPEndToEnd drives a full site lifecycle through MCP and checks the
// result through the public site handler.
func TestMCPEndToEnd(t *testing.T) {
	srv, h := newTestServer(t)
	tok := seedScopedKey(t, srv, model.ScopeMCP)

	created := mustTool(t, h, tok, "create_project", map[string]any{"name": "Docs", "slug": "docs"})
	id := created["id"].(string)
	if created["site_url"] != "/docs/" {
		t.Fatalf("site_url = %v", created["site_url"])
	}

	// Duplicate slug and invalid input are tool errors the model can read.
	if isErr, _, text := mcpTool(t, h, tok, "create_project", map[string]any{"name": "x", "slug": "docs"}); !isErr || !strings.Contains(text, "already in use") {
		t.Fatalf("duplicate slug: isErr=%v %s", isErr, text)
	}
	if isErr, _, text := mcpTool(t, h, tok, "create_project", map[string]any{"name": "x", "slug": "x", "colour": "red"}); !isErr || !strings.Contains(text, "unknown field") {
		t.Fatalf("unknown arg: isErr=%v %s", isErr, text)
	}

	// Text upload by slug.
	w := mustTool(t, h, tok, "write_file", map[string]any{"project": "docs", "path": "index.html", "content": "<h1>Hello MCP</h1>"})
	if w["created"] != false { // the placeholder index existed
		t.Fatalf("write_file index created = %v, want false (overwrite)", w["created"])
	}

	// Binary batch upload by id (PNG header bytes are not valid UTF-8).
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0x00}
	up := mustTool(t, h, tok, "upload_files", map[string]any{"project": id, "files": []map[string]string{
		{"path": "img/logo.png", "content": base64.StdEncoding.EncodeToString(png), "encoding": "base64"},
		{"path": "css/site.css", "content": "body{color:red}"},
		{"path": "../escape.txt", "content": "nope"},
	}})
	if n := len(up["uploaded"].([]any)); n != 2 {
		t.Fatalf("uploaded %d, want 2: %v", n, up)
	}
	failed := up["failed"].([]any)
	if len(failed) != 1 || !strings.Contains(failed[0].(map[string]any)["error"].(string), "invalid") {
		t.Fatalf("failed = %v", failed)
	}

	rf := mustTool(t, h, tok, "read_file", map[string]any{"project": "docs", "path": "img/logo.png"})
	if rf["encoding"] != "base64" || rf["content"] != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("read_file binary = %v", rf)
	}

	// Zip upload preserving folders.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for name, body := range map[string]string{"about/index.html": "<p>about</p>", "js/app.js": "console.log(1)"} {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(body))
	}
	_ = zw.Close()
	z := mustTool(t, h, tok, "upload_zip", map[string]any{"project": "docs", "zip_base64": base64.StdEncoding.EncodeToString(zbuf.Bytes())})
	if z["files_extracted"] != float64(2) {
		t.Fatalf("upload_zip = %v", z)
	}
	if isErr, _, _ := mcpTool(t, h, tok, "upload_zip", map[string]any{"project": "docs", "zip_base64": base64.StdEncoding.EncodeToString([]byte("not a zip"))}); !isErr {
		t.Fatal("garbage zip should be a tool error")
	}

	// Rename the landing page: index_file follows.
	mustTool(t, h, tok, "rename_file", map[string]any{"project": "docs", "from": "index.html", "to": "home.html"})
	p := mustTool(t, h, tok, "get_project", map[string]any{"project": "docs"})
	if p["index_file"] != "home.html" {
		t.Fatalf("index_file after rename = %v", p["index_file"])
	}

	// The public site serves what MCP uploaded.
	for path, want := range map[string]string{"/docs/": "Hello MCP", "/docs/css/site.css": "color:red", "/docs/about/": "about"} {
		rr := get(h, path)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("GET %s = %d %q", path, rr.Code, rr.Body.String())
		}
	}

	files := mustTool(t, h, tok, "list_files", map[string]any{"project": "docs"})
	if n := len(files["files"].([]any)); n != 5 {
		t.Fatalf("list_files = %d files, want 5: %v", n, files)
	}

	mustTool(t, h, tok, "delete_file", map[string]any{"project": "docs", "path": "js/app.js"})
	if isErr, _, _ := mcpTool(t, h, tok, "read_file", map[string]any{"project": "docs", "path": "js/app.js"}); !isErr {
		t.Fatal("deleted file still readable")
	}

	mustTool(t, h, tok, "update_project", map[string]any{"project": "docs", "status": "unlinked"})
	if rr := get(h, "/docs/"); rr.Code != http.StatusNotFound {
		t.Fatalf("unlinked site served: %d", rr.Code)
	}

	mustTool(t, h, tok, "delete_project", map[string]any{"project": "docs"})
	if isErr, _, text := mcpTool(t, h, tok, "get_project", map[string]any{"project": id}); !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("deleted project: %s", text)
	}
	list := mustTool(t, h, tok, "list_projects", map[string]any{})
	if n := len(list["projects"].([]any)); n != 0 {
		t.Fatalf("projects left: %d", n)
	}
}

func TestSettingsCreateMCPTokenShowsSnippets(t *testing.T) {
	srv, h := newTestServer(t)
	cookie := srv.auth.IssueSessionCookie()

	form := url.Values{"name": {"claude"}, "scope": {"mcp"}, "csrf_token": {srv.auth.IssueCSRFToken()}}
	rr := postTo(h, "/_app/settings/api-keys", form, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"claude mcp add --transport http paperplane http://example.com/api/mcp", "paperplane mcp", "MCP only"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	keys, _ := srv.store.ListAPIKeys(context.Background())
	if len(keys) != 1 || keys[0].Scope != model.ScopeMCP {
		t.Fatalf("stored keys = %+v", keys)
	}

	// API-only tokens do not get MCP snippets; bogus scopes are rejected.
	form = url.Values{"name": {"ci"}, "scope": {"api"}, "csrf_token": {srv.auth.IssueCSRFToken()}}
	if body := postTo(h, "/_app/settings/api-keys", form, cookie).Body.String(); strings.Contains(body, "claude mcp add") {
		t.Error("api-only token should not show MCP snippets")
	}
	form = url.Values{"name": {"x"}, "scope": {"root"}, "csrf_token": {srv.auth.IssueCSRFToken()}}
	if body := postTo(h, "/_app/settings/api-keys", form, cookie).Body.String(); !strings.Contains(body, "valid scope") {
		t.Error("invalid scope should re-render with an error")
	}
}
