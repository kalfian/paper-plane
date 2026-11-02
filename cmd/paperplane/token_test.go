package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kalfian/paper-plane/internal/apikey"
	"github.com/kalfian/paper-plane/internal/config"
	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/store"
)

func TestTokenCmd(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "pp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg := config.Config{AppURL: "https://sites.example.com", Port: "8080"}

	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := runTokenCmd(ctx, st, cfg, args, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	code, out, errOut := run("create", "--name", "claude", "--scope", "mcp")
	if code != 0 {
		t.Fatalf("create: %d %s", code, errOut)
	}
	token := strings.TrimSpace(out)
	if !strings.HasPrefix(token, apikey.Prefix) {
		t.Fatalf("stdout should be just the token, got %q", out)
	}
	if !strings.Contains(errOut, "https://sites.example.com/api/mcp") {
		t.Fatalf("missing MCP hint: %s", errOut)
	}
	k, err := st.GetAPIKeyByHash(ctx, apikey.Hash(token))
	if err != nil || k.Scope != model.ScopeMCP || k.Name != "claude" {
		t.Fatalf("stored = %+v, %v", k, err)
	}

	if code, _, _ := run("create", "--name", "x", "--scope", "admin"); code != 2 {
		t.Fatalf("bad scope exit = %d", code)
	}
	if code, _, _ := run("create"); code != 2 {
		t.Fatalf("missing name exit = %d", code)
	}

	code, out, _ = run("list")
	if code != 0 || !strings.Contains(out, "claude") || !strings.Contains(out, "mcp") || strings.Contains(out, token) {
		t.Fatalf("list: %d %q", code, out)
	}

	if code, _, _ := run("revoke", k.ID); code != 0 {
		t.Fatalf("revoke exit = %d", code)
	}
	if code, _, _ := run("revoke", k.ID); code != 1 {
		t.Fatalf("second revoke exit = %d", code)
	}
}

func TestMCPEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"https://sites.example.com":         "https://sites.example.com/api/mcp",
		"https://sites.example.com/":        "https://sites.example.com/api/mcp",
		"https://sites.example.com/api/mcp": "https://sites.example.com/api/mcp",
		"http://localhost:8080/api/mcp/":    "http://localhost:8080/api/mcp",
		"https://example.com/pp":            "https://example.com/pp/api/mcp",
	} {
		got, err := mcpEndpoint(in)
		if err != nil || got != want {
			t.Errorf("mcpEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "sites.example.com", "ftp://x"} {
		if _, err := mcpEndpoint(bad); err == nil {
			t.Errorf("mcpEndpoint(%q) should fail", bad)
		}
	}
}
