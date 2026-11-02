package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

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
	URL       string    `json:"url,omitempty"` // absolute site URL; only when APP_URL is set
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

// requireAPIKey authenticates a REST API request via the Authorization: Bearer
// header and requires a key whose scope grants the REST API.
func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return s.requireKeyScope(model.KeyScope.AllowsAPI, "This key is not allowed to use the REST API (scope: MCP only).", next)
}

// requireKeyScope authenticates a request via the Authorization: Bearer header
// and checks the key's scope with allowed. On success it records the key's
// last-used time (best-effort) and calls next. On failure it writes a 401 (bad
// or missing key, with WWW-Authenticate) or 403 (valid key, wrong scope) JSON
// error.
func (s *Server) requireKeyScope(allowed func(model.KeyScope) bool, forbiddenMsg string, next http.Handler) http.Handler {
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
		if !allowed(key.Scope) {
			s.writeAPIError(w, http.StatusForbidden, "forbidden", forbiddenMsg)
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
		"mcp": "POST " + mcpPath + " (Model Context Protocol, Streamable HTTP; same bearer keys with an MCP scope)",
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
		URL:       s.absoluteSiteURL(p.Slug),
		FileCount: count,
		Size:      size,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

// absoluteSiteURL returns APP_URL + "/<slug>/", or "" when APP_URL is unset.
func (s *Server) absoluteSiteURL(slug string) string {
	if s.cfg.AppURL == "" {
		return ""
	}
	return s.cfg.AppURL + "/" + slug + "/"
}

// loadProjectAPI fetches the {id} project, writing a JSON 404/500 and returning
// ok=false when it cannot be served.
func (s *Server) loadProjectAPI(w http.ResponseWriter, r *http.Request) (*model.Project, bool) {
	p, err := s.getProjectByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeOpError(w, err)
		return nil, false
	}
	return p, true
}

// writeOpError writes an operation error (see ops.go) as the JSON envelope.
func (s *Server) writeOpError(w http.ResponseWriter, err error) {
	oe := asOpError(err)
	s.writeAPIError(w, oe.Status, oe.Code, oe.Msg)
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
	p, err := s.createProject(r.Context(), req.Name, req.Slug, req.Status)
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.toAPIProject(*p))
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
	updated, err := s.updateProject(r.Context(), p, projectPatch(req))
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.toAPIProject(*updated))
}

// handleAPIProjectDelete: DELETE /api/v1/projects/{id}.
func (s *Server) handleAPIProjectDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteProject(r.Context(), r.PathValue("id")); err != nil {
		s.writeOpError(w, err)
		return
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
	files, err := s.listFiles(p.ID)
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// handleAPIFileGet: GET /api/v1/projects/{id}/files/{path...}. Returns the file
// content as UTF-8 text when valid, otherwise base64.
func (s *Server) handleAPIFileGet(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProjectAPI(w, r)
	if !ok {
		return
	}
	resp, err := s.readFile(p.ID, r.PathValue("path"))
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleAPIFilePut: PUT /api/v1/projects/{id}/files/{path...}. Creates (201) or
// overwrites (200) the file with the decoded content.
func (s *Server) handleAPIFilePut(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProjectAPI(w, r)
	if !ok {
		return
	}
	if _, err := requirePath(r.PathValue("path")); err != nil {
		s.writeOpError(w, err)
		return
	}
	var req fileWriteReq
	if !s.decodeJSON(w, r, &req) {
		return
	}
	f, created, err := s.writeFile(p.ID, r.PathValue("path"), req.Content, req.Encoding)
	if err != nil {
		s.writeOpError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, f)
}

// handleAPIFileDelete: DELETE /api/v1/projects/{id}/files/{path...}.
func (s *Server) handleAPIFileDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProjectAPI(w, r)
	if !ok {
		return
	}
	if err := s.deleteFile(p.ID, r.PathValue("path")); err != nil {
		s.writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
