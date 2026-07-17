package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kalfian/paper-plane/internal/apikey"
	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/sitefs"
	"github.com/kalfian/paper-plane/internal/store"
)

// The REST API lives under /api/v1/*. It is authenticated with a bearer API key
// (Authorization: Bearer <key>) instead of the cookie session + CSRF that guard
// the /_app/* admin UI: API clients are non-browser (AI agents, scripts), so
// there is no cookie to forge and CSRF does not apply. Every request and
// response body is JSON. Errors use a consistent envelope:
//
//	{"error": {"code": "<machine-code>", "message": "<human message>"}}

// maxAPIBody caps a single JSON request body. It is roughly double the site
// upload budget so a base64-encoded file (≈+33%) up to the per-file limit still
// fits, with headroom for the JSON envelope.
const maxAPIBody = 2*sitefs.DefaultMaxUploadBytes + (1 << 20)

// --- JSON response shapes ---

// apiProject is the JSON representation of a project returned by the API.
type apiProject struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Status    string    `json:"status"`
	IndexFile string    `json:"index_file"`
	SiteURL   string    `json:"site_url"`
	FileCount int       `json:"file_count"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// apiFile is the JSON representation of a single file entry (metadata only).
type apiFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// apiFileContent is returned when reading a file: the bytes are UTF-8 text when
// possible (encoding "utf8"), otherwise base64 ("base64").
type apiFileContent struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Encoding string `json:"encoding"` // "utf8" | "base64"
	Content  string `json:"content"`
}

// apiErrorBody is the error envelope.
type apiErrorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// --- request shapes ---

// projectCreateReq is the POST /projects body.
type projectCreateReq struct {
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
}

// projectUpdateReq is the PATCH /projects/{id} body. Pointer fields distinguish
// "omitted" (nil, leave unchanged) from "set to this value".
type projectUpdateReq struct {
	Name      *string `json:"name"`
	Status    *string `json:"status"`
	IndexFile *string `json:"index_file"`
}

// fileWriteReq is the PUT /files/{path} body.
type fileWriteReq struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"` // "utf8" (default) | "base64"
}

// --- JSON helpers ---

// writeJSON encodes v as JSON with the given status.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("encode json response", "error", err)
	}
}

// writeAPIError writes the error envelope with the given status.
func (s *Server) writeAPIError(w http.ResponseWriter, status int, code, msg string) {
	s.writeJSON(w, status, apiErrorBody{Error: apiError{Code: code, Message: msg}})
}

// decodeJSON reads and strictly decodes the request body into dst, enforcing the
// body-size cap and rejecting unknown fields. It writes an error response and
// returns false on any problem.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			s.writeAPIError(w, http.StatusRequestEntityTooLarge, "too_large", "Request body exceeds the size limit.")
			return false
		}
		s.writeAPIError(w, http.StatusBadRequest, "invalid_request", "Request body is not valid JSON: "+err.Error())
		return false
	}
	return true
}

// --- auth middleware ---

// requireAPIKey authenticates a request via the Authorization: Bearer header. On
// success it records the key's last-used time (best-effort) and calls next. On
// failure it writes a 401 JSON error and sets WWW-Authenticate.
func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := apikey.FromAuthorizationHeader(r.Header.Get("Authorization"))
		if token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			s.writeAPIError(w, http.StatusUnauthorized, "unauthorized", "Missing or malformed Authorization header. Use: Authorization: Bearer <api-key>.")
			return
		}
		key, err := s.store.GetAPIKeyByHash(r.Context(), apikey.Hash(token))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				s.writeAPIError(w, http.StatusUnauthorized, "unauthorized", "Invalid API key.")
				return
			}
			s.log.Error("lookup api key", "error", err)
			s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
			return
		}
		// Best-effort usage tracking; never fail the request on a touch error.
		if err := s.store.TouchAPIKey(r.Context(), key.ID); err != nil {
			s.log.Error("touch api key", "id", key.ID, "error", err)
		}
		next.ServeHTTP(w, r)
	})
}

// --- discovery ---

// handleAPIIndex is a public (no auth) discovery endpoint describing the API. It
// is registered on both "GET /api/v1" and the "GET /api/v1/" subtree, so it also
// catches otherwise-unmatched paths under /api/v1/; those get a JSON 404 rather
// than the discovery doc.
func (s *Server) handleAPIIndex(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Path; p != "/api/v1" && p != "/api/v1/" {
		s.writeAPIError(w, http.StatusNotFound, "not_found", "Unknown API endpoint.")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"name":    "Paper Plane API",
		"version": "v1",
		"auth":    "Send 'Authorization: Bearer <api-key>'. Create keys in the admin UI under Settings.",
		"resources": map[string]string{
			"projects": "/api/v1/projects",
			"files":    "/api/v1/projects/{id}/files",
		},
	})
}

// --- project handlers ---

// toAPIProject builds the JSON view of a project, computing file count and size.
func (s *Server) toAPIProject(p model.Project) apiProject {
	var (
		count int
		size  int64
	)
	files, err := s.fs.ListFiles(p.ID)
	if err != nil {
		s.log.Error("list files for api project", "id", p.ID, "error", err)
	} else {
		count = len(files)
		for _, f := range files {
			size += f.Size
		}
	}
	return apiProject{
		ID:        p.ID,
		Name:      p.Name,
		Slug:      p.Slug,
		Status:    string(p.Status),
		IndexFile: p.EffectiveIndexFile(),
		SiteURL:   "/" + p.Slug + "/",
		FileCount: count,
		Size:      size,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

// loadProjectAPI fetches the {id} project, writing a JSON 404/500 and returning
// ok=false when it cannot be served.
func (s *Server) loadProjectAPI(w http.ResponseWriter, r *http.Request) (*model.Project, bool) {
	id := r.PathValue("id")
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeAPIError(w, http.StatusNotFound, "not_found", "Project not found.")
			return nil, false
		}
		s.log.Error("api get project", "id", id, "error", err)
		s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
		return nil, false
	}
	return p, true
}

// handleAPIProjectsList: GET /api/v1/projects.
func (s *Server) handleAPIProjectsList(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		s.log.Error("api list projects", "error", err)
		s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
		return
	}
	out := make([]apiProject, 0, len(projects))
	for _, p := range projects {
		out = append(out, s.toAPIProject(p))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

// handleAPIProjectGet: GET /api/v1/projects/{id}.
func (s *Server) handleAPIProjectGet(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProjectAPI(w, r)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, s.toAPIProject(*p))
}

// handleAPIProjectCreate: POST /api/v1/projects.
func (s *Server) handleAPIProjectCreate(w http.ResponseWriter, r *http.Request) {
	var req projectCreateReq
	if !s.decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	slug := strings.TrimSpace(req.Slug)
	if name == "" {
		s.writeAPIError(w, http.StatusUnprocessableEntity, "validation_error", "Name is required.")
		return
	}
	if msg := validateSlug(slug); msg != "" {
		s.writeAPIError(w, http.StatusUnprocessableEntity, "validation_error", msg)
		return
	}

	status := model.StatusActive
	if req.Status != "" {
		status = model.Status(req.Status)
		if !status.Valid() {
			s.writeAPIError(w, http.StatusUnprocessableEntity, "validation_error", `Status must be "active" or "unlinked".`)
			return
		}
	}

	p := &model.Project{ID: store.NewID(), Name: name, Slug: slug, Status: status}
	if err := s.store.CreateProject(r.Context(), p); err != nil {
		if errors.Is(err, store.ErrSlugExists) {
			s.writeAPIError(w, http.StatusConflict, "slug_exists", "That slug is already in use.")
			return
		}
		s.log.Error("api create project", "error", err)
		s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
		return
	}
	if err := s.fs.CreateSite(p.ID); err != nil {
		s.log.Error("api create site dir", "id", p.ID, "error", err)
		_ = s.store.DeleteProject(r.Context(), p.ID) // roll back the orphan row
		s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
		return
	}
	// Give the empty site a placeholder landing page so it serves immediately.
	if err := s.fs.WritePlaceholderIndex(p.ID); err != nil {
		s.log.Error("api write placeholder", "id", p.ID, "error", err)
	}

	// Re-fetch to return canonical timestamps/state.
	created, err := s.store.GetProject(r.Context(), p.ID)
	if err != nil {
		s.log.Error("api reload created project", "id", p.ID, "error", err)
		s.writeJSON(w, http.StatusCreated, s.toAPIProject(*p))
		return
	}
	s.writeJSON(w, http.StatusCreated, s.toAPIProject(*created))
}

// handleAPIProjectUpdate: PATCH /api/v1/projects/{id}. Only provided fields
// change (name, status, index_file).
func (s *Server) handleAPIProjectUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProjectAPI(w, r)
	if !ok {
		return
	}
	var req projectUpdateReq
	if !s.decodeJSON(w, r, &req) {
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			s.writeAPIError(w, http.StatusUnprocessableEntity, "validation_error", "Name must not be empty.")
			return
		}
		p.Name = name
		if err := s.store.UpdateProject(r.Context(), p); err != nil {
			s.log.Error("api update project name", "id", p.ID, "error", err)
			s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
			return
		}
	}

	if req.Status != nil {
		status := model.Status(strings.TrimSpace(*req.Status))
		if !status.Valid() {
			s.writeAPIError(w, http.StatusUnprocessableEntity, "validation_error", `Status must be "active" or "unlinked".`)
			return
		}
		if err := s.store.SetStatus(r.Context(), p.ID, status); err != nil {
			s.log.Error("api set status", "id", p.ID, "error", err)
			s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
			return
		}
	}

	if req.IndexFile != nil {
		idx := strings.TrimSpace(*req.IndexFile)
		// A non-empty index must reference an existing file; empty resets to the
		// default (index.html).
		if idx != "" && !s.fileExists(p.ID, idx) {
			s.writeAPIError(w, http.StatusUnprocessableEntity, "validation_error", "index_file does not exist in this project.")
			return
		}
		if err := s.store.SetIndexFile(r.Context(), p.ID, idx); err != nil {
			s.log.Error("api set index file", "id", p.ID, "error", err)
			s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
			return
		}
	}

	updated, err := s.store.GetProject(r.Context(), p.ID)
	if err != nil {
		s.log.Error("api reload updated project", "id", p.ID, "error", err)
		s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
		return
	}
	s.writeJSON(w, http.StatusOK, s.toAPIProject(*updated))
}

// handleAPIProjectDelete: DELETE /api/v1/projects/{id}.
func (s *Server) handleAPIProjectDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteProject(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeAPIError(w, http.StatusNotFound, "not_found", "Project not found.")
			return
		}
		s.log.Error("api delete project", "id", id, "error", err)
		s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
		return
	}
	if err := s.fs.DeleteSite(id); err != nil {
		s.log.Error("api delete site dir", "id", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- file handlers ---

// handleAPIFilesList: GET /api/v1/projects/{id}/files.
func (s *Server) handleAPIFilesList(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProjectAPI(w, r)
	if !ok {
		return
	}
	files, err := s.fs.ListFiles(p.ID)
	if err != nil {
		s.log.Error("api list files", "id", p.ID, "error", err)
		s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
		return
	}
	out := make([]apiFile, 0, len(files))
	for _, f := range files {
		out = append(out, apiFile{Path: f.Path, Size: f.Size})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

// handleAPIFileGet: GET /api/v1/projects/{id}/files/{path...}. Returns the file
// content as UTF-8 text when valid, otherwise base64.
func (s *Server) handleAPIFileGet(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProjectAPI(w, r)
	if !ok {
		return
	}
	relpath := r.PathValue("path")
	if strings.TrimSpace(relpath) == "" {
		s.writeAPIError(w, http.StatusBadRequest, "invalid_request", "A file path is required.")
		return
	}
	data, err := s.fs.ReadFile(p.ID, relpath)
	if err != nil {
		s.writeFileError(w, err)
		return
	}
	resp := apiFileContent{Path: relpath, Size: int64(len(data))}
	if utf8.Valid(data) {
		resp.Encoding = "utf8"
		resp.Content = string(data)
	} else {
		resp.Encoding = "base64"
		resp.Content = base64.StdEncoding.EncodeToString(data)
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleAPIFilePut: PUT /api/v1/projects/{id}/files/{path...}. Creates or
// overwrites the file with the decoded content.
func (s *Server) handleAPIFilePut(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProjectAPI(w, r)
	if !ok {
		return
	}
	relpath := r.PathValue("path")
	if strings.TrimSpace(relpath) == "" {
		s.writeAPIError(w, http.StatusBadRequest, "invalid_request", "A file path is required.")
		return
	}

	var req fileWriteReq
	if !s.decodeJSON(w, r, &req) {
		return
	}

	var data []byte
	switch enc := strings.ToLower(strings.TrimSpace(req.Encoding)); enc {
	case "", "utf8", "utf-8", "text":
		data = []byte(req.Content)
	case "base64":
		decoded, derr := base64.StdEncoding.DecodeString(req.Content)
		if derr != nil {
			s.writeAPIError(w, http.StatusBadRequest, "invalid_request", "content is not valid base64.")
			return
		}
		data = decoded
	default:
		s.writeAPIError(w, http.StatusBadRequest, "invalid_request", `encoding must be "utf8" or "base64".`)
		return
	}

	if int64(len(data)) > sitefs.DefaultMaxUploadBytes {
		s.writeAPIError(w, http.StatusRequestEntityTooLarge, "too_large", "File content exceeds the size limit.")
		return
	}

	// Distinguish create (201) from overwrite (200).
	existed := s.fileExists(p.ID, relpath)

	if err := s.fs.WriteFile(p.ID, relpath, data); err != nil {
		s.writeFileError(w, err)
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	s.writeJSON(w, status, apiFile{Path: relpath, Size: int64(len(data))})
}

// handleAPIFileDelete: DELETE /api/v1/projects/{id}/files/{path...}.
func (s *Server) handleAPIFileDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProjectAPI(w, r)
	if !ok {
		return
	}
	relpath := r.PathValue("path")
	if strings.TrimSpace(relpath) == "" {
		s.writeAPIError(w, http.StatusBadRequest, "invalid_request", "A file path is required.")
		return
	}
	if err := s.fs.DeleteFile(p.ID, relpath); err != nil {
		s.writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeFileError maps a sitefs/filesystem error to a JSON API error.
func (s *Server) writeFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sitefs.ErrUnsafePath):
		s.writeAPIError(w, http.StatusBadRequest, "unsafe_path", "The file path is invalid (path traversal or absolute paths are not allowed).")
	case errors.Is(err, fs.ErrNotExist):
		s.writeAPIError(w, http.StatusNotFound, "not_found", "File not found.")
	case errors.Is(err, sitefs.ErrTooLarge):
		s.writeAPIError(w, http.StatusRequestEntityTooLarge, "too_large", "File content exceeds the size limit.")
	default:
		s.log.Error("api file operation", "error", err)
		s.writeAPIError(w, http.StatusInternalServerError, "internal", "Internal server error.")
	}
}
