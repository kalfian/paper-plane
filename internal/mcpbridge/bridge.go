// Package mcpbridge implements the local side of Paper Plane's MCP support: a
// stdio MCP server (run as `paperplane mcp`) that proxies every remote tool to
// a Paper Plane instance and adds upload_local_path, which reads files or whole
// folders from the local disk and uploads them — so an AI client can publish a
// build directory without base64-encoding anything itself.
//
// Local reads are confined to Root (default: the working directory the bridge
// was started in). Hidden files and folders (".git", ".env", ...) and symlinks
// are skipped unless explicitly allowed, so a prompt-injected request cannot
// casually publish secrets from elsewhere on the machine.
package mcpbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kalfian/paper-plane/internal/mcp"
)

// Limits for one upload_local_path call.
const (
	// MaxFileBytes mirrors the server's per-file limit.
	MaxFileBytes int64 = 50 << 20
	// MaxFiles caps the number of files gathered from a directory.
	MaxFiles = 5000
	// MaxTotalBytes caps the total bytes uploaded by one call.
	MaxTotalBytes int64 = 1 << 30
	// batchBytes / batchFiles bound one upstream upload_files request.
	batchBytes int64 = 16 << 20
	batchFiles       = 200
)

// LocalUploadTool is the name of the bridge-only tool.
const LocalUploadTool = "upload_local_path"

// Upstream is the remote MCP server the bridge forwards to (an *mcp.Client).
type Upstream interface {
	ListTools(ctx context.Context) ([]mcp.Tool, error)
	CallTool(ctx context.Context, name string, args any) (*mcp.CallToolResult, error)
}

// Bridge is an mcp.ToolProvider: remote tools plus upload_local_path.
type Bridge struct {
	Upstream Upstream
	// Root confines local reads; must be an absolute, existing directory.
	Root string
}

var _ mcp.ToolProvider = (*Bridge)(nil)

var localUploadTool = mcp.Tool{
	Name:  LocalUploadTool,
	Title: "Upload local file or folder",
	Description: "Upload a file or a whole folder from this machine's disk to a Paper Plane project (no base64 needed). " +
		"For a folder, its CONTENTS are uploaded (e.g. local_path \"dist\" puts dist/index.html at <dest_dir>/index.html), preserving sub-folders. " +
		"Existing files with the same path are overwritten. Hidden files/folders (.git, .env, ...) and symlinks are skipped unless include_hidden is true. " +
		"Paths are resolved relative to, and must stay inside, the bridge's root directory.",
	InputSchema: json.RawMessage(`{"type": "object", "properties": {
		"project": {"type": "string", "description": "Project id or slug."},
		"local_path": {"type": "string", "description": "Local file or folder, relative to the bridge root or absolute inside it."},
		"dest_dir": {"type": "string", "description": "Folder inside the site to upload into. Default: the site root."},
		"include_hidden": {"type": "boolean", "default": false, "description": "Also upload dot-files and dot-folders."}
	}, "required": ["project", "local_path"], "additionalProperties": false}`),
	Annotations: &mcp.ToolAnnotations{IdempotentHint: mcp.Bool(true)},
}

// ListTools returns the remote tools followed by upload_local_path.
func (b *Bridge) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	tools, err := b.Upstream.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("reach Paper Plane: %w", err)
	}
	out := make([]mcp.Tool, 0, len(tools)+1)
	for _, t := range tools {
		if t.Name != LocalUploadTool {
			out = append(out, t)
		}
	}
	return append(out, localUploadTool), nil
}

// CallTool runs upload_local_path locally and forwards everything else.
func (b *Bridge) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	if name == LocalUploadTool {
		return b.uploadLocalPath(ctx, args)
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, err := b.Upstream.CallTool(ctx, name, args)
	if err != nil {
		var rpcErr *mcp.RPCError
		if errors.As(err, &rpcErr) {
			return nil, rpcErr // keep protocol errors (e.g. unknown tool) as-is
		}
		return nil, err
	}
	return res, nil
}

type uploadArgs struct {
	Project       string `json:"project"`
	LocalPath     string `json:"local_path"`
	DestDir       string `json:"dest_dir"`
	IncludeHidden bool   `json:"include_hidden"`
}

// localFile is one file scheduled for upload.
type localFile struct {
	abs  string // absolute local path
	dest string // slash-separated site path
	size int64
}

type uploadedFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type failedFile struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

func (b *Bridge) uploadLocalPath(ctx context.Context, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a uploadArgs
	if err := mcp.DecodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Project) == "" {
		return nil, errors.New("project is required")
	}
	files, skipped, err := b.collect(a)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files to upload at %q (%d hidden/symlinked entries skipped)", a.LocalPath, skipped)
	}

	var (
		uploaded = []uploadedFile{}
		failed   = []failedFile{}
		total    int64
		siteURL  string
	)
	for _, batch := range batches(files) {
		payload := make([]map[string]string, 0, len(batch))
		for _, f := range batch {
			data, rerr := os.ReadFile(f.abs)
			if rerr != nil {
				failed = append(failed, failedFile{Path: f.dest, Error: "read local file: " + rerr.Error()})
				continue
			}
			payload = append(payload, map[string]string{
				"path":     f.dest,
				"content":  base64.StdEncoding.EncodeToString(data),
				"encoding": "base64",
			})
		}
		if len(payload) == 0 {
			continue
		}
		res, cerr := b.Upstream.CallTool(ctx, "upload_files", map[string]any{"project": a.Project, "files": payload})
		if cerr != nil {
			for _, p := range payload {
				failed = append(failed, failedFile{Path: p["path"], Error: cerr.Error()})
			}
			if ctx.Err() != nil {
				break
			}
			continue
		}
		var out struct {
			Uploaded []uploadedFile `json:"uploaded"`
			Failed   []failedFile   `json:"failed"`
			SiteURL  string         `json:"site_url"`
		}
		if !decodeResult(res, &out) {
			msg := resultText(res)
			for _, p := range payload {
				failed = append(failed, failedFile{Path: p["path"], Error: msg})
			}
			continue
		}
		uploaded = append(uploaded, out.Uploaded...)
		failed = append(failed, out.Failed...)
		for _, u := range out.Uploaded {
			total += u.Size
		}
		if out.SiteURL != "" {
			siteURL = out.SiteURL
		}
	}

	result := mcp.JSONResult(map[string]any{
		"project":        a.Project,
		"uploaded_count": len(uploaded),
		"uploaded":       uploaded,
		"failed":         failed,
		"skipped_hidden": skipped,
		"total_bytes":    total,
		"site_url":       siteURL,
	})
	result.IsError = len(uploaded) == 0
	return result, nil
}

// collect resolves local_path inside Root and gathers the files to upload.
func (b *Bridge) collect(a uploadArgs) (files []localFile, skipped int, err error) {
	abs, err := b.resolve(a.LocalPath)
	if err != nil {
		return nil, 0, err
	}
	destDir, err := cleanDestDir(a.DestDir)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, 0, fmt.Errorf("local path: %w", err)
	}

	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil, 0, fmt.Errorf("%q is not a regular file", a.LocalPath)
		}
		if info.Size() > MaxFileBytes {
			return nil, 0, fmt.Errorf("%q is larger than the 50 MiB per-file limit", a.LocalPath)
		}
		return []localFile{{abs: abs, dest: path.Join(destDir, filepath.Base(abs)), size: info.Size()}}, 0, nil
	}

	var total int64
	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == abs {
			return nil
		}
		if !a.IncludeHidden && strings.HasPrefix(d.Name(), ".") {
			skipped++
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			skipped++
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		rel, rerr := filepath.Rel(abs, p)
		if rerr != nil {
			return rerr
		}
		if fi.Size() > MaxFileBytes {
			return fmt.Errorf("%s is larger than the 50 MiB per-file limit", rel)
		}
		total += fi.Size()
		if total > MaxTotalBytes {
			return fmt.Errorf("folder is larger than the %d MiB per-call limit; upload sub-folders separately", MaxTotalBytes>>20)
		}
		files = append(files, localFile{abs: p, dest: path.Join(destDir, filepath.ToSlash(rel)), size: fi.Size()})
		if len(files) > MaxFiles {
			return fmt.Errorf("folder has more than %d files; upload sub-folders separately", MaxFiles)
		}
		return nil
	})
	if walkErr != nil {
		return nil, skipped, walkErr
	}
	return files, skipped, nil
}

// resolve turns a user-supplied path into an absolute path that is inside Root
// after symlink resolution.
func (b *Bridge) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("local_path is required")
	}
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(b.Root, p)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("local path: %w", err)
	}
	root, err := filepath.EvalSymlinks(b.Root)
	if err != nil {
		return "", fmt.Errorf("bridge root: %w", err)
	}
	if !within(root, real) {
		return "", fmt.Errorf("%q is outside the bridge root %q; restart the bridge with --root (or PAPERPLANE_ROOT) to allow it", p, b.Root)
	}
	return real, nil
}

// within reports whether p is root or inside it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// cleanDestDir normalizes the destination folder ("" = site root) and rejects
// traversal; the server validates again.
func cleanDestDir(d string) (string, error) {
	d = strings.Trim(strings.TrimSpace(filepath.ToSlash(d)), "/")
	if d == "" || d == "." {
		return "", nil
	}
	c := path.Clean(d)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", errors.New("dest_dir must stay inside the site")
	}
	return c, nil
}

// batches splits files into upstream-sized groups.
func batches(files []localFile) [][]localFile {
	var (
		out  [][]localFile
		cur  []localFile
		size int64
	)
	for _, f := range files {
		if len(cur) > 0 && (len(cur) >= batchFiles || size+f.size > batchBytes) {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, f)
		size += f.size
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// decodeResult extracts a JSON object from a tool result (structuredContent or
// the first text item). It returns false when the result is an error without a
// JSON body.
func decodeResult(res *mcp.CallToolResult, dst any) bool {
	if res == nil {
		return false
	}
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil && json.Unmarshal(b, dst) == nil {
			return true
		}
	}
	for _, c := range res.Content {
		if c.Type == "text" && json.Unmarshal([]byte(c.Text), dst) == nil {
			return true
		}
	}
	return false
}

func resultText(res *mcp.CallToolResult) string {
	if res == nil {
		return "empty result"
	}
	var parts []string
	for _, c := range res.Content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	if len(parts) == 0 {
		return "upload failed"
	}
	return strings.Join(parts, "\n")
}
