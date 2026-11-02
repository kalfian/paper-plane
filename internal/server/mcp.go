package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kalfian/paper-plane/internal/mcp"
	"github.com/kalfian/paper-plane/internal/model"
)

// MCP (Model Context Protocol) endpoint. It exposes the same project/file
// operations as the REST API (through ops.go) as MCP tools, so AI clients such
// as Claude Code, Claude Desktop, or Cursor can manage and upload sites
// directly. Transport: stateless Streamable HTTP at POST /api/mcp. Auth: the
// same bearer access tokens as the REST API, with a scope that includes MCP.

// mcpPath is where the MCP endpoint is mounted ("api" is a reserved slug, so no
// site can shadow it).
const mcpPath = "/api/mcp"

// Version is reported as the MCP serverInfo version. cmd/paperplane may
// override it at build time.
var Version = "dev"

// maxUploadBatchFiles caps the number of files in one upload_files call.
const maxUploadBatchFiles = 500

const mcpInstructions = `Paper Plane hosts static websites. Each project is one site served at <base-url>/<slug>/ (see each project's "url" or "site_url").

Typical flow: list_projects (or create_project) → upload content → open the site URL.
- Projects can be referenced by id or slug in every tool's "project" argument.
- Text files (HTML/CSS/JS/JSON/SVG/MD...) go through write_file or upload_files with encoding "utf8".
- Binary files (images, fonts, PDFs) must be base64 with encoding "base64".
- To publish a whole folder at once, prefer upload_zip (a base64 .zip; relative paths are preserved), or upload_files for a handful of files.
- If you are running through the local Paper Plane bridge, upload_local_path uploads files/folders straight from disk without base64.
- The landing page is index.html unless update_project sets index_file.
- delete_project and delete_file are permanent.`

// mcpHandler builds the authenticated MCP HTTP handler.
func (s *Server) mcpHandler() http.Handler {
	srv := &mcp.Server{
		Info:         mcp.Implementation{Name: "paper-plane", Title: "Paper Plane", Version: Version},
		Instructions: mcpInstructions,
		Tools:        s.mcpTools(),
		Log:          s.log,
	}
	h := &mcp.HTTPHandler{Server: srv, MaxBodyBytes: maxAPIBody}
	return s.requireKeyScope(model.KeyScope.AllowsMCP, "This key is not allowed to use MCP (scope: REST API only).", h)
}

// schema is a tiny helper for writing JSON Schemas inline.
func schema(s string) json.RawMessage { return json.RawMessage(s) }

const projectRefProp = `"project": {"type": "string", "description": "Project id or slug."}`

// mcpTools registers every Paper Plane tool.
func (s *Server) mcpTools() *mcp.Registry {
	reg := &mcp.Registry{}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: mcp.Bool(true)}

	// --- projects ---

	reg.Add(mcp.Tool{
		Name:        "list_projects",
		Title:       "List projects",
		Description: "List every hosted static-site project with its id, slug, status, URL, file count, and total size.",
		InputSchema: schema(`{"type": "object", "properties": {}, "additionalProperties": false}`),
		Annotations: ro,
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		if err := mcp.DecodeArgs(args, &struct{}{}); err != nil {
			return nil, err
		}
		projects, err := s.store.ListProjects(ctx)
		if err != nil {
			s.log.Error("mcp list projects", "error", err)
			return nil, errInternalOp
		}
		out := make([]apiProject, 0, len(projects))
		for _, p := range projects {
			out = append(out, s.toAPIProject(p))
		}
		return mcp.JSONResult(map[string]any{"projects": out}), nil
	})

	reg.Add(mcp.Tool{
		Name:        "get_project",
		Title:       "Get project",
		Description: "Get one project by id or slug.",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `}, "required": ["project"], "additionalProperties": false}`),
		Annotations: ro,
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct {
			Project string `json:"project"`
		}
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		return mcp.JSONResult(s.toAPIProject(*p)), nil
	})

	reg.Add(mcp.Tool{
		Name:  "create_project",
		Title: "Create project",
		Description: "Create a new static-site project. It starts with a placeholder index.html and is served at /<slug>/. " +
			"Slug: lowercase letters, digits, and hyphens.",
		InputSchema: schema(`{"type": "object", "properties": {
			"name": {"type": "string", "description": "Display name."},
			"slug": {"type": "string", "description": "URL path segment, e.g. \"my-site\" → /my-site/. Immutable."},
			"status": {"type": "string", "enum": ["active", "unlinked"], "description": "Default active. Unlinked sites return 404 to visitors."}
		}, "required": ["name", "slug"], "additionalProperties": false}`),
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct{ Name, Slug, Status string }
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.createProject(ctx, a.Name, a.Slug, a.Status)
		if err != nil {
			return nil, err
		}
		return mcp.JSONResult(s.toAPIProject(*p)), nil
	})

	reg.Add(mcp.Tool{
		Name:        "update_project",
		Title:       "Update project",
		Description: "Rename a project, change its status (active/unlinked), or set its landing page (index_file; empty string resets to index.html). Only provided fields change.",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `,
			"name": {"type": "string"},
			"status": {"type": "string", "enum": ["active", "unlinked"]},
			"index_file": {"type": "string", "description": "Path of an existing file to serve at the site root."}
		}, "required": ["project"], "additionalProperties": false}`),
		Annotations: &mcp.ToolAnnotations{IdempotentHint: mcp.Bool(true)},
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct {
			Project   string  `json:"project"`
			Name      *string `json:"name"`
			Status    *string `json:"status"`
			IndexFile *string `json:"index_file"`
		}
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		updated, err := s.updateProject(ctx, p, projectPatch{Name: a.Name, Status: a.Status, IndexFile: a.IndexFile})
		if err != nil {
			return nil, err
		}
		return mcp.JSONResult(s.toAPIProject(*updated)), nil
	})

	reg.Add(mcp.Tool{
		Name:        "delete_project",
		Title:       "Delete project",
		Description: "Permanently delete a project and ALL of its files. Cannot be undone. Consider update_project with status \"unlinked\" to take a site offline instead.",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `}, "required": ["project"], "additionalProperties": false}`),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: mcp.Bool(true)},
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct {
			Project string `json:"project"`
		}
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		if err := s.deleteProject(ctx, p.ID); err != nil {
			return nil, err
		}
		return mcp.JSONResult(map[string]any{"deleted": true, "id": p.ID, "slug": p.Slug}), nil
	})

	// --- files ---

	reg.Add(mcp.Tool{
		Name:        "list_files",
		Title:       "List files",
		Description: "List every file in a project (relative path and size in bytes).",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `}, "required": ["project"], "additionalProperties": false}`),
		Annotations: ro,
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct {
			Project string `json:"project"`
		}
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		files, err := s.listFiles(p.ID)
		if err != nil {
			return nil, err
		}
		return mcp.JSONResult(map[string]any{"project": p.Slug, "index_file": p.EffectiveIndexFile(), "files": files}), nil
	})

	reg.Add(mcp.Tool{
		Name:        "read_file",
		Title:       "Read file",
		Description: "Read a file. Text is returned as UTF-8 (encoding \"utf8\"); binary files as base64 (encoding \"base64\").",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `,
			"path": {"type": "string", "description": "Relative path inside the site, e.g. \"css/site.css\"."}
		}, "required": ["project", "path"], "additionalProperties": false}`),
		Annotations: ro,
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct{ Project, Path string }
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		f, err := s.readFile(p.ID, a.Path)
		if err != nil {
			return nil, err
		}
		return mcp.JSONResult(f), nil
	})

	reg.Add(mcp.Tool{
		Name:  "write_file",
		Title: "Write / upload file",
		Description: "Create or overwrite one file (parent folders are created). Use encoding \"utf8\" for text and \"base64\" for binary files such as images. " +
			"Max 50 MiB per file.",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `,
			"path": {"type": "string", "description": "Relative path inside the site, e.g. \"index.html\" or \"img/logo.png\"."},
			"content": {"type": "string", "description": "File content: plain text, or base64 when encoding is base64."},
			"encoding": {"type": "string", "enum": ["utf8", "base64"], "default": "utf8"}
		}, "required": ["project", "path", "content"], "additionalProperties": false}`),
		Annotations: &mcp.ToolAnnotations{IdempotentHint: mcp.Bool(true)},
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct{ Project, Path, Content, Encoding string }
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		f, created, err := s.writeFile(p.ID, a.Path, a.Content, a.Encoding)
		if err != nil {
			return nil, err
		}
		return mcp.JSONResult(map[string]any{"path": f.Path, "size": f.Size, "created": created, "url": s.fileURL(p, f.Path)}), nil
	})

	reg.Add(mcp.Tool{
		Name:  "upload_files",
		Title: "Upload files",
		Description: "Upload several files in one call (each with its own path and encoding). Files that fail are reported individually; the rest are still written. " +
			"Up to 500 files and ~50 MiB of decoded content per call.",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `,
			"files": {"type": "array", "minItems": 1, "items": {"type": "object", "properties": {
				"path": {"type": "string"},
				"content": {"type": "string"},
				"encoding": {"type": "string", "enum": ["utf8", "base64"], "default": "utf8"}
			}, "required": ["path", "content"], "additionalProperties": false}}
		}, "required": ["project", "files"], "additionalProperties": false}`),
		Annotations: &mcp.ToolAnnotations{IdempotentHint: mcp.Bool(true)},
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct {
			Project string `json:"project"`
			Files   []struct {
				Path, Content, Encoding string
			} `json:"files"`
		}
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		if len(a.Files) == 0 {
			return nil, errBadRequest("files must contain at least one file.")
		}
		if len(a.Files) > maxUploadBatchFiles {
			return nil, errBadRequest("Too many files in one call (max 500); split the upload or use upload_zip.")
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		type failure struct {
			Path  string `json:"path"`
			Error string `json:"error"`
		}
		uploaded := make([]apiFile, 0, len(a.Files))
		failed := []failure{}
		var total int64
		for _, f := range a.Files {
			wf, _, werr := s.writeFile(p.ID, f.Path, f.Content, f.Encoding)
			if werr != nil {
				failed = append(failed, failure{Path: f.Path, Error: asOpError(werr).Msg})
				continue
			}
			uploaded = append(uploaded, wf)
			total += wf.Size
		}
		res := mcp.JSONResult(map[string]any{
			"project":     p.Slug,
			"uploaded":    uploaded,
			"failed":      failed,
			"total_bytes": total,
			"site_url":    s.fileURL(p, ""),
		})
		res.IsError = len(uploaded) == 0
		return res, nil
	})

	reg.Add(mcp.Tool{
		Name:  "upload_zip",
		Title: "Upload zip archive",
		Description: "Upload a base64-encoded .zip and extract it into the project, preserving relative paths (existing files with the same path are overwritten). " +
			"Best way to publish a whole built site. Limits: 50 MiB uncompressed, 500 entries.",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `,
			"zip_base64": {"type": "string", "description": "The .zip file, base64-encoded."}
		}, "required": ["project", "zip_base64"], "additionalProperties": false}`),
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct {
			Project   string `json:"project"`
			ZipBase64 string `json:"zip_base64"`
		}
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		n, err := s.extractZip(p.ID, a.ZipBase64)
		if err != nil {
			return nil, err
		}
		return mcp.JSONResult(map[string]any{"project": p.Slug, "files_extracted": n, "site_url": s.fileURL(p, "")}), nil
	})

	reg.Add(mcp.Tool{
		Name:        "rename_file",
		Title:       "Rename / move file",
		Description: "Rename or move a file within a project. Never overwrites an existing file. If the landing page is moved, the project's index_file follows it.",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `,
			"from": {"type": "string", "description": "Current relative path."},
			"to": {"type": "string", "description": "New relative path (may include folders)."}
		}, "required": ["project", "from", "to"], "additionalProperties": false}`),
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct{ Project, From, To string }
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		if err := s.renameFile(ctx, p, a.From, a.To); err != nil {
			return nil, err
		}
		return mcp.JSONResult(map[string]any{"from": strings.TrimSpace(a.From), "to": strings.TrimSpace(a.To)}), nil
	})

	reg.Add(mcp.Tool{
		Name:        "delete_file",
		Title:       "Delete file",
		Description: "Permanently delete one file from a project.",
		InputSchema: schema(`{"type": "object", "properties": {` + projectRefProp + `,
			"path": {"type": "string"}
		}, "required": ["project", "path"], "additionalProperties": false}`),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: mcp.Bool(true)},
	}, func(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
		var a struct{ Project, Path string }
		if err := mcp.DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		p, err := s.resolveProject(ctx, a.Project)
		if err != nil {
			return nil, err
		}
		if err := s.deleteFile(p.ID, a.Path); err != nil {
			return nil, err
		}
		return mcp.JSONResult(map[string]any{"deleted": strings.TrimSpace(a.Path)}), nil
	})

	return reg
}

// fileURL returns the public URL of a file in p (the site root when relpath is
// empty): absolute when APP_URL is configured, otherwise root-relative.
func (s *Server) fileURL(p *model.Project, relpath string) string {
	return s.cfg.AppURL + "/" + p.Slug + "/" + strings.TrimLeft(relpath, "/")
}
