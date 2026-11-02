package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/sitefs"
	"github.com/kalfian/paper-plane/internal/store"
)

// This file holds the transport-neutral project/file operations shared by the
// REST API (api.go) and the MCP endpoint (mcp.go). Each operation validates its
// input and returns either a result or an *opError carrying the HTTP status,
// machine code, and human message, so both transports report failures
// identically. Unexpected errors are logged here and surfaced as "internal".

// opError is a client-facing operation failure.
type opError struct {
	Status int    // HTTP status the REST API responds with
	Code   string // machine-readable code, e.g. "not_found"
	Msg    string // human-readable message
}

func (e *opError) Error() string { return e.Code + ": " + e.Msg }

func errValidation(msg string) *opError {
	return &opError{Status: http.StatusUnprocessableEntity, Code: "validation_error", Msg: msg}
}

func errBadRequest(msg string) *opError {
	return &opError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: msg}
}

var (
	errProjectNotFound = &opError{Status: http.StatusNotFound, Code: "not_found", Msg: "Project not found."}
	errInternalOp      = &opError{Status: http.StatusInternalServerError, Code: "internal", Msg: "Internal server error."}
)

// asOpError converts any error into an *opError, mapping anything unknown to
// "internal" (the caller is expected to have logged it).
func asOpError(err error) *opError {
	if oe, ok := errors.AsType[*opError](err); ok {
		return oe
	}
	return errInternalOp
}

// fileOpError maps a sitefs/filesystem error to an *opError, logging
// unexpected failures.
func (s *Server) fileOpError(err error) *opError {
	switch {
	case errors.Is(err, sitefs.ErrUnsafePath), errors.Is(err, sitefs.ErrUnsafeEntry):
		return &opError{Status: http.StatusBadRequest, Code: "unsafe_path", Msg: "The file path is invalid (path traversal or absolute paths are not allowed)."}
	case errors.Is(err, fs.ErrNotExist):
		return &opError{Status: http.StatusNotFound, Code: "not_found", Msg: "File not found."}
	case errors.Is(err, fs.ErrExist):
		return &opError{Status: http.StatusConflict, Code: "file_exists", Msg: "A file already exists at the destination path."}
	case errors.Is(err, sitefs.ErrTooLarge):
		return &opError{Status: http.StatusRequestEntityTooLarge, Code: "too_large", Msg: "File content exceeds the size limit."}
	case errors.Is(err, sitefs.ErrTooManyEntries):
		return &opError{Status: http.StatusRequestEntityTooLarge, Code: "too_large", Msg: "Zip has too many entries."}
	default:
		s.log.Error("file operation", "error", err)
		return errInternalOp
	}
}

// requirePath trims and rejects an empty file path.
func requirePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errBadRequest("A file path is required.")
	}
	return p, nil
}

// --- projects ---

// getProjectByID loads a project by id.
func (s *Server) getProjectByID(ctx context.Context, id string) (*model.Project, error) {
	p, err := s.store.GetProject(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errProjectNotFound
		}
		s.log.Error("get project", "id", id, "error", err)
		return nil, errInternalOp
	}
	return p, nil
}

// resolveProject loads a project by id, falling back to slug. It lets MCP
// clients refer to a project by whichever identifier they have at hand.
func (s *Server) resolveProject(ctx context.Context, ref string) (*model.Project, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errBadRequest("A project id or slug is required.")
	}
	p, err := s.getProjectByID(ctx, ref)
	if err == nil {
		return p, nil
	}
	if asOpError(err) != errProjectNotFound {
		return nil, err
	}
	p, serr := s.store.GetProjectBySlug(ctx, ref)
	if serr != nil {
		if errors.Is(serr, store.ErrNotFound) {
			return nil, errProjectNotFound
		}
		s.log.Error("get project by slug", "slug", ref, "error", serr)
		return nil, errInternalOp
	}
	return p, nil
}

// createProject validates input, inserts the project row, creates its site
// directory, and writes the placeholder index. It rolls the row back if the
// directory cannot be created.
func (s *Server) createProject(ctx context.Context, name, slug, status string) (*model.Project, error) {
	name = strings.TrimSpace(name)
	slug = strings.TrimSpace(slug)
	if name == "" {
		return nil, errValidation("Name is required.")
	}
	if msg := validateSlug(slug); msg != "" {
		return nil, errValidation(msg)
	}
	st := model.StatusActive
	if status != "" {
		st = model.Status(status)
		if !st.Valid() {
			return nil, errValidation(`Status must be "active" or "unlinked".`)
		}
	}

	p := &model.Project{ID: store.NewID(), Name: name, Slug: slug, Status: st}
	if err := s.store.CreateProject(ctx, p); err != nil {
		if errors.Is(err, store.ErrSlugExists) {
			return nil, &opError{Status: http.StatusConflict, Code: "slug_exists", Msg: "That slug is already in use."}
		}
		s.log.Error("create project", "error", err)
		return nil, errInternalOp
	}
	if err := s.fs.CreateSite(p.ID); err != nil {
		s.log.Error("create site dir", "id", p.ID, "error", err)
		_ = s.store.DeleteProject(ctx, p.ID) // roll back the orphan row
		return nil, errInternalOp
	}
	// Give the empty site a placeholder landing page so it serves immediately.
	if err := s.fs.WritePlaceholderIndex(p.ID); err != nil {
		s.log.Error("write placeholder", "id", p.ID, "error", err)
	}
	// Re-fetch to return canonical timestamps/state.
	created, err := s.store.GetProject(ctx, p.ID)
	if err != nil {
		s.log.Error("reload created project", "id", p.ID, "error", err)
		return p, nil
	}
	return created, nil
}

// projectPatch carries optional project updates; nil fields are left unchanged.
type projectPatch struct {
	Name      *string
	Status    *string
	IndexFile *string
}

// updateProject applies the non-nil fields of patch to p and returns the
// reloaded project. A non-empty index file must exist; empty resets it to the
// default (index.html).
func (s *Server) updateProject(ctx context.Context, p *model.Project, patch projectPatch) (*model.Project, error) {
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" {
			return nil, errValidation("Name must not be empty.")
		}
		p.Name = name
		if err := s.store.UpdateProject(ctx, p); err != nil {
			s.log.Error("update project name", "id", p.ID, "error", err)
			return nil, errInternalOp
		}
	}
	if patch.Status != nil {
		st := model.Status(strings.TrimSpace(*patch.Status))
		if !st.Valid() {
			return nil, errValidation(`Status must be "active" or "unlinked".`)
		}
		if err := s.store.SetStatus(ctx, p.ID, st); err != nil {
			s.log.Error("set status", "id", p.ID, "error", err)
			return nil, errInternalOp
		}
	}
	if patch.IndexFile != nil {
		idx := strings.TrimSpace(*patch.IndexFile)
		if idx != "" && !s.fileExists(p.ID, idx) {
			return nil, errValidation("index_file does not exist in this project.")
		}
		if err := s.store.SetIndexFile(ctx, p.ID, idx); err != nil {
			s.log.Error("set index file", "id", p.ID, "error", err)
			return nil, errInternalOp
		}
	}
	updated, err := s.store.GetProject(ctx, p.ID)
	if err != nil {
		s.log.Error("reload updated project", "id", p.ID, "error", err)
		return nil, errInternalOp
	}
	return updated, nil
}

// deleteProject removes the project row and (best-effort) its site files.
func (s *Server) deleteProject(ctx context.Context, id string) error {
	if err := s.store.DeleteProject(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errProjectNotFound
		}
		s.log.Error("delete project", "id", id, "error", err)
		return errInternalOp
	}
	if err := s.fs.DeleteSite(id); err != nil {
		s.log.Error("delete site dir", "id", id, "error", err)
	}
	return nil
}

// --- files ---

// listFiles returns the project's files as API entries.
func (s *Server) listFiles(id string) ([]apiFile, error) {
	files, err := s.fs.ListFiles(id)
	if err != nil {
		s.log.Error("list files", "id", id, "error", err)
		return nil, errInternalOp
	}
	out := make([]apiFile, 0, len(files))
	for _, f := range files {
		out = append(out, apiFile{Path: f.Path, Size: f.Size})
	}
	return out, nil
}

// readFile returns a file's content as UTF-8 text when valid, else base64.
func (s *Server) readFile(id, relpath string) (apiFileContent, error) {
	relpath, err := requirePath(relpath)
	if err != nil {
		return apiFileContent{}, err
	}
	data, err := s.fs.ReadFile(id, relpath)
	if err != nil {
		return apiFileContent{}, s.fileOpError(err)
	}
	resp := apiFileContent{Path: relpath, Size: int64(len(data))}
	if utf8.Valid(data) {
		resp.Encoding = "utf8"
		resp.Content = string(data)
	} else {
		resp.Encoding = "base64"
		resp.Content = base64.StdEncoding.EncodeToString(data)
	}
	return resp, nil
}

// decodeContent decodes content per encoding ("utf8" default, or "base64").
func decodeContent(content, encoding string) ([]byte, error) {
	switch enc := strings.ToLower(strings.TrimSpace(encoding)); enc {
	case "", "utf8", "utf-8", "text":
		return []byte(content), nil
	case "base64":
		// Tolerate whitespace/newlines that encoders commonly insert.
		cleaned := strings.Join(strings.Fields(content), "")
		decoded, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return nil, errBadRequest("content is not valid base64.")
		}
		return decoded, nil
	default:
		return nil, errBadRequest(`encoding must be "utf8" or "base64".`)
	}
}

// writeFile decodes and writes one file, reporting whether it was newly created
// (false = overwritten).
func (s *Server) writeFile(id, relpath, content, encoding string) (apiFile, bool, error) {
	relpath, err := requirePath(relpath)
	if err != nil {
		return apiFile{}, false, err
	}
	data, err := decodeContent(content, encoding)
	if err != nil {
		return apiFile{}, false, err
	}
	if int64(len(data)) > sitefs.DefaultMaxUploadBytes {
		return apiFile{}, false, s.fileOpError(sitefs.ErrTooLarge)
	}
	existed := s.fileExists(id, relpath)
	if err := s.fs.WriteFile(id, relpath, data); err != nil {
		return apiFile{}, false, s.fileOpError(err)
	}
	return apiFile{Path: relpath, Size: int64(len(data))}, !existed, nil
}

// deleteFile removes one file.
func (s *Server) deleteFile(id, relpath string) error {
	relpath, err := requirePath(relpath)
	if err != nil {
		return err
	}
	if err := s.fs.DeleteFile(id, relpath); err != nil {
		return s.fileOpError(err)
	}
	return nil
}

// renameFile moves a file to a new relative path (which may change directory)
// without overwriting. When the landing page moves, index_file follows it.
func (s *Server) renameFile(ctx context.Context, p *model.Project, from, to string) error {
	from, err := requirePath(from)
	if err != nil {
		return err
	}
	to, err = requirePath(to)
	if err != nil {
		return err
	}
	if path.Clean(from) == path.Clean(to) {
		return errValidation("The new path is the same as the current path.")
	}
	if err := s.fs.Rename(p.ID, from, to); err != nil {
		return s.fileOpError(err)
	}
	if path.Clean(from) == path.Clean(p.EffectiveIndexFile()) {
		if err := s.store.SetIndexFile(ctx, p.ID, path.Clean(to)); err != nil {
			s.log.Error("update index after rename", "id", p.ID, "error", err)
		}
	}
	return nil
}

// extractZip decodes a base64 zip archive and extracts it into the site,
// preserving relative paths. It returns the number of files written.
func (s *Server) extractZip(id, zipBase64 string) (int, error) {
	data, err := decodeContent(zipBase64, "base64")
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, errBadRequest("zip archive is empty.")
	}
	n, err := s.fs.ExtractZip(id, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		switch {
		case errors.Is(err, sitefs.ErrUnsafePath), errors.Is(err, sitefs.ErrUnsafeEntry),
			errors.Is(err, sitefs.ErrTooLarge), errors.Is(err, sitefs.ErrTooManyEntries):
			return n, s.fileOpError(err)
		default:
			// Most likely a malformed archive rather than a server fault.
			s.log.Warn("extract zip", "id", id, "files_written", n, "error", err)
			return n, errBadRequest(fmt.Sprintf("Could not extract the zip archive (%d files written before the failure).", n))
		}
	}
	return n, nil
}
