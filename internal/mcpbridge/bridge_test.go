package mcpbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/apikey"
	"github.com/kalfian/paper-plane/internal/config"
	"github.com/kalfian/paper-plane/internal/mcp"
	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/server"
	"github.com/kalfian/paper-plane/internal/sitefs"
	"github.com/kalfian/paper-plane/internal/store"
)

// newInstance starts a real Paper Plane HTTP server and returns its URL and an
// MCP-scoped token.
func newInstance(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.NewSQLite(ctx, filepath.Join(dir, "pp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := server.New(ctx, config.Config{DataDir: dir, Port: "0"}, st, sitefs.New(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	token, hash := apikey.Generate()
	if err := st.CreateAPIKey(ctx, &model.APIKey{ID: store.NewID(), Name: "bridge", KeyHash: hash, Scope: model.ScopeMCP}); err != nil {
		t.Fatal(err)
	}
	return ts.URL, token
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// stdioSession drives the bridge exactly as an MCP client would: JSON-RPC
// lines over stdio.
func stdioSession(t *testing.T, b *Bridge, msgs ...string) []map[string]any {
	t.Helper()
	srv := &mcp.Server{Info: mcp.Implementation{Name: "bridge", Version: "test"}, Tools: b}
	var out strings.Builder
	if err := srv.ServeStdio(context.Background(), strings.NewReader(strings.Join(msgs, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		res = append(res, m)
	}
	return res
}

func toolCall(id int, name string, args any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	return string(b)
}

func resultPayload(t *testing.T, m map[string]any) (bool, map[string]any, string) {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	var p map[string]any
	_ = json.Unmarshal([]byte(text), &p)
	isErr, _ := r["isError"].(bool)
	return isErr, p, text
}

func TestBridgeEndToEnd(t *testing.T) {
	base, token := newInstance(t)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"dist/index.html":      "<h1>from disk</h1>",
		"dist/assets/app.css":  "body{margin:0}",
		"dist/.env":            "SECRET=1",
		"dist/.git/config":     "[core]",
		"notes/readme.txt":     "single file",
		"dist/sub/deep/x.json": `{"ok":true}`,
	})
	// A symlink inside the folder pointing outside root must be skipped.
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret.txt": "nope"})
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "dist", "leak.txt")); err != nil {
		t.Fatal(err)
	}

	client := &mcp.Client{URL: base + "/api/mcp", Token: token, Info: mcp.Implementation{Name: "test", Version: "1"}}
	b := &Bridge{Upstream: client, Root: root}

	res := stdioSession(t, b,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		toolCall(3, "create_project", map[string]any{"name": "Local", "slug": "local"}),
		toolCall(4, LocalUploadTool, map[string]any{"project": "local", "local_path": "dist"}),
		toolCall(5, LocalUploadTool, map[string]any{"project": "local", "local_path": filepath.Join(root, "notes", "readme.txt"), "dest_dir": "docs"}),
		toolCall(6, LocalUploadTool, map[string]any{"project": "local", "local_path": filepath.Join(outside, "secret.txt")}),
		toolCall(7, LocalUploadTool, map[string]any{"project": "local", "local_path": "../" + filepath.Base(outside)}),
		toolCall(8, "list_files", map[string]any{"project": "local"}),
	)
	if len(res) != 8 {
		t.Fatalf("want 8 responses, got %d", len(res))
	}

	// tools/list = remote tools + upload_local_path.
	names := map[string]bool{}
	for _, tl := range res[1]["result"].(map[string]any)["tools"].([]any) {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	if !names["write_file"] || !names["upload_zip"] || !names[LocalUploadTool] {
		t.Fatalf("tools = %v", names)
	}

	if isErr, _, text := resultPayload(t, res[2]); isErr {
		t.Fatalf("create_project via bridge: %s", text)
	}

	isErr, up, text := resultPayload(t, res[3])
	if isErr || up["uploaded_count"] != float64(3) {
		t.Fatalf("upload dist: %s", text)
	}
	if up["skipped_hidden"] != float64(3) { // .env, .git/, leak.txt symlink
		t.Fatalf("skipped = %v, want 3", up["skipped_hidden"])
	}

	if isErr, _, text := resultPayload(t, res[4]); isErr {
		t.Fatalf("upload single file: %s", text)
	}
	for i, idx := range []int{5, 6} {
		if isErr, _, text := resultPayload(t, res[idx]); !isErr || !strings.Contains(text, "outside the bridge root") {
			t.Fatalf("outside-root case %d: isErr=%v %s", i, isErr, text)
		}
	}

	_, lf, _ := resultPayload(t, res[7])
	got := map[string]bool{}
	for _, f := range lf["files"].([]any) {
		got[f.(map[string]any)["path"].(string)] = true
	}
	for _, want := range []string{"index.html", "assets/app.css", "sub/deep/x.json", "docs/readme.txt"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	for _, bad := range []string{".env", ".git/config", "leak.txt"} {
		if got[bad] {
			t.Errorf("%s should not have been uploaded", bad)
		}
	}

	if code, body := fetch(t, base+"/local/"); code != 200 || !strings.Contains(body, "from disk") {
		t.Fatalf("site root = %d %q", code, body)
	}
	if code, body := fetch(t, base+"/local/docs/readme.txt"); code != 200 || body != "single file" {
		t.Fatalf("docs/readme.txt = %d %q", code, body)
	}
}

func TestBridgeReportsUpstreamAuthFailure(t *testing.T) {
	base, _ := newInstance(t)
	b := &Bridge{Upstream: &mcp.Client{URL: base + "/api/mcp", Token: "pp_wrong"}, Root: t.TempDir()}
	res := stdioSession(t, b, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	e, ok := res[0]["error"].(map[string]any)
	if !ok || !strings.Contains(e["message"].(string), "401") {
		t.Fatalf("want a 401 error, got %v", res[0])
	}
}

func TestBatchesAndDestDir(t *testing.T) {
	var files []localFile
	for range 450 {
		files = append(files, localFile{size: 1})
	}
	files = append(files, localFile{size: batchBytes}, localFile{size: 1})
	bs := batches(files)
	if len(bs) != 5 || len(bs[0]) != batchFiles || len(bs[3]) != 1 {
		t.Fatalf("batch sizes: %d batches", len(bs))
	}
	for in, want := range map[string]string{"": "", "/": "", "a/b/": "a/b", "./x": "x"} {
		if got, err := cleanDestDir(in); err != nil || got != want {
			t.Errorf("cleanDestDir(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := cleanDestDir("../up"); err == nil {
		t.Error("cleanDestDir(../up) should fail")
	}
}
