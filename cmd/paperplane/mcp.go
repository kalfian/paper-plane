package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kalfian/paper-plane/internal/mcp"
	"github.com/kalfian/paper-plane/internal/mcpbridge"
	"github.com/kalfian/paper-plane/internal/server"
)

// mcpCmd runs the local stdio MCP bridge. Configuration comes from flags, with
// PAPERPLANE_URL / PAPERPLANE_TOKEN / PAPERPLANE_ROOT as fallbacks (so the
// token need not appear in a process listing). stdout carries only protocol
// messages; diagnostics go to stderr.
func mcpCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fset.SetOutput(stderr)
	rawURL := fset.String("url", os.Getenv("PAPERPLANE_URL"), "Paper Plane base URL or MCP endpoint (env PAPERPLANE_URL)")
	token := fset.String("token", os.Getenv("PAPERPLANE_TOKEN"), "access token with MCP scope (env PAPERPLANE_TOKEN)")
	root := fset.String("root", os.Getenv("PAPERPLANE_ROOT"), "directory upload_local_path may read from (env PAPERPLANE_ROOT; default: current directory)")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))

	endpoint, err := mcpEndpoint(*rawURL)
	if err != nil {
		log.Error("invalid --url", "error", err)
		return 2
	}
	if strings.TrimSpace(*token) == "" {
		log.Error("an access token is required: set PAPERPLANE_TOKEN or pass --token (generate one in Settings or with `paperplane token create --scope mcp`)")
		return 2
	}
	rootDir := *root
	if rootDir == "" {
		if rootDir, err = os.Getwd(); err != nil {
			log.Error("resolve working directory", "error", err)
			return 1
		}
	}
	if rootDir, err = filepath.Abs(rootDir); err != nil {
		log.Error("resolve --root", "error", err)
		return 2
	}
	if fi, serr := os.Stat(rootDir); serr != nil || !fi.IsDir() {
		log.Error("--root must be an existing directory", "root", rootDir)
		return 2
	}

	client := &mcp.Client{
		URL:   endpoint,
		Token: strings.TrimSpace(*token),
		Info:  mcp.Implementation{Name: "paperplane-bridge", Version: server.Version},
		HTTP:  &http.Client{Timeout: 10 * time.Minute},
	}
	srv := &mcp.Server{
		Info: mcp.Implementation{Name: "paper-plane-local", Title: "Paper Plane (local bridge)", Version: server.Version},
		Instructions: "Paper Plane static-site host, via a local bridge. All remote tools are available; additionally upload_local_path " +
			"uploads files or folders from this machine (inside " + rootDir + ") without base64. Prefer it for publishing build output.",
		Tools: &mcpbridge.Bridge{Upstream: client, Root: rootDir},
		Log:   log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("paperplane MCP bridge ready", "endpoint", endpoint, "root", rootDir)
	if err := srv.ServeStdio(ctx, stdin, stdout); err != nil {
		log.Error("stdio", "error", err)
		return 1
	}
	return 0
}

// mcpEndpoint accepts either the instance base URL or the full endpoint URL and
// returns the MCP endpoint.
func mcpEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("set PAPERPLANE_URL or pass --url (e.g. https://sites.example.com)")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not an http(s) URL", raw)
	}
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, "/api/mcp") {
		p += "/api/mcp"
	}
	u.Path = p
	return u.String(), nil
}
