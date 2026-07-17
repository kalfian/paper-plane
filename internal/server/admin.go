package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/store"
)

// maxSlugLen is the maximum allowed slug length.
const maxSlugLen = 63

// slugRe matches a valid slug: starts with a lowercase letter or digit, then
// lowercase letters, digits, or hyphens.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// reservedSlugs are slugs that must not be used because they would collide with
// application routes or are otherwise reserved.
var reservedSlugs = map[string]bool{
	"_app":    true,
	"api":     true,
	"healthz": true,
	"static":  true,
	"assets":  true,
}

// validateSlug reports a friendly error string ("" when valid).
func validateSlug(slug string) string {
	switch {
	case slug == "":
		return "Slug is required."
	case len(slug) > maxSlugLen:
		return fmt.Sprintf("Slug must be at most %d characters.", maxSlugLen)
	case !slugRe.MatchString(slug):
		return "Slug may contain only lowercase letters, digits, and hyphens, and must start with a letter or digit."
	case reservedSlugs[slug]:
		return "That slug is reserved. Please choose another."
	default:
		return ""
	}
}

// --- template data structs (see web/templates/CONTRACT.md) ---

// projectView is the per-project row shown in list and detail templates.
type projectView struct {
	ID        string
	Name      string
	Slug      string
	Status    string // "active" | "unlinked"
	Active    bool
	FileCount int
	Size      int64
	SizeHuman string
	SiteURL   string // path to the live site, e.g. "/<slug>/"
	IndexFile string // filename served at the site root
	UpdatedAt time.Time
}

// projectsData is passed to projects.html.
type projectsData struct {
	CSRFToken string
	Projects  []projectView
	Flash     string
	Error     string
}

// projectNewData is passed to project_new.html.
type projectNewData struct {
	CSRFToken string
	Name      string // prefilled on validation error
	Slug      string
	Error     string
}

// projectEditData is passed to project_edit.html.
type projectEditData struct {
	CSRFToken string
	Project   projectView
	Flash     string
	Error     string
}

// newProjectView builds a projectView, computing file count and size.
func (s *Server) newProjectView(p model.Project) projectView {
	files, err := s.fs.ListFiles(p.ID)
	if err != nil {
		s.log.Error("list files for view", "id", p.ID, "error", err)
	}
	var size int64
	for _, f := range files {
		size += f.Size
	}
	return projectView{
		ID:        p.ID,
		Name:      p.Name,
		Slug:      p.Slug,
		Status:    string(p.Status),
		Active:    p.Status == model.StatusActive,
		FileCount: len(files),
		Size:      size,
		SizeHuman: humanBytes(size),
		SiteURL:   "/" + p.Slug + "/",
		IndexFile: p.EffectiveIndexFile(),
		UpdatedAt: p.UpdatedAt,
	}
}

// handleProjectsList renders the project list. Registered for GET /_app/{$},
// GET /_app/projects.
func (s *Server) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		s.log.Error("list projects", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	views := make([]projectView, 0, len(projects))
	for _, p := range projects {
		views = append(views, s.newProjectView(p))
	}
	s.rd.render(w, http.StatusOK, "projects.html", projectsData{
		CSRFToken: s.auth.IssueCSRFToken(),
		Projects:  views,
		Flash:     r.URL.Query().Get("flash"),
	})
}

// handleProjectNew renders the create form.
func (s *Server) handleProjectNew(w http.ResponseWriter, _ *http.Request) {
	s.rd.render(w, http.StatusOK, "project_new.html", projectNewData{
		CSRFToken: s.auth.IssueCSRFToken(),
	})
}

// handleProjectCreate validates input, creates the DB row and site directory,
// processes any optional upload, and redirects to the file manager. On
// validation failure it re-renders the form with an error.
func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := r.ParseMultipartForm(uploadParseMemory); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		s.renderCreateError(w, "", "", "Could not read the submitted form.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	slug := strings.TrimSpace(r.FormValue("slug"))

	if name == "" {
		s.renderCreateError(w, name, slug, "Name is required.")
		return
	}
	if msg := validateSlug(slug); msg != "" {
		s.renderCreateError(w, name, slug, msg)
		return
	}

	p := &model.Project{ID: store.NewID(), Name: name, Slug: slug, Status: model.StatusActive}
	if err := s.store.CreateProject(r.Context(), p); err != nil {
		if errors.Is(err, store.ErrSlugExists) {
			s.renderCreateError(w, name, slug, "That slug is already in use.")
			return
		}
		s.log.Error("create project", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := s.fs.CreateSite(p.ID); err != nil {
		s.log.Error("create site dir", "id", p.ID, "error", err)
		_ = s.store.DeleteProject(r.Context(), p.ID) // roll back the orphan row
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	added, upErr := s.processUploads(p.ID, r)
	if upErr != nil {
		// The project exists; surface the upload problem on the file manager.
		http.Redirect(w, r, s.filesURL(p.ID, "flash="+uploadErrorMessage(upErr)), http.StatusSeeOther)
		return
	}
	if added == 0 {
		if err := s.fs.WritePlaceholderIndex(p.ID); err != nil {
			s.log.Error("write placeholder", "id", p.ID, "error", err)
		}
	} else if name := loneHTMLUpload(r); name != "" {
		// A single uploaded HTML page becomes the site's landing page, kept under
		// its own name (no rename to index.html).
		if err := s.store.SetIndexFile(r.Context(), p.ID, name); err != nil {
			s.log.Error("set index file", "id", p.ID, "name", name, "error", err)
		}
	}

	http.Redirect(w, r, s.filesURL(p.ID, "flash=Project+created."), http.StatusSeeOther)
}

// renderCreateError re-renders the create form with an error (HTTP 200).
func (s *Server) renderCreateError(w http.ResponseWriter, name, slug, msg string) {
	s.rd.render(w, http.StatusOK, "project_new.html", projectNewData{
		CSRFToken: s.auth.IssueCSRFToken(),
		Name:      name,
		Slug:      slug,
		Error:     msg,
	})
}

// handleProjectEdit renders the rename form for a single project.
func (s *Server) handleProjectEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	s.rd.render(w, http.StatusOK, "project_edit.html", projectEditData{
		CSRFToken: s.auth.IssueCSRFToken(),
		Project:   s.newProjectView(*p),
		Flash:     r.URL.Query().Get("flash"),
	})
}

// handleProjectUpdate updates the project name (slug is immutable in MVP).
func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.rd.render(w, http.StatusOK, "project_edit.html", projectEditData{
			CSRFToken: s.auth.IssueCSRFToken(),
			Project:   s.newProjectView(*p),
			Error:     "Name is required.",
		})
		return
	}
	p.Name = name
	if err := s.store.UpdateProject(r.Context(), p); err != nil {
		s.log.Error("update project", "id", p.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/_app/projects/"+p.ID+"?flash=Saved.", http.StatusSeeOther)
}

// handleProjectUnlink sets status=unlinked (site returns 404).
func (s *Server) handleProjectUnlink(w http.ResponseWriter, r *http.Request) {
	s.setStatusAndRedirect(w, r, model.StatusUnlinked)
}

// handleProjectRelink sets status=active.
func (s *Server) handleProjectRelink(w http.ResponseWriter, r *http.Request) {
	s.setStatusAndRedirect(w, r, model.StatusActive)
}

// setStatusAndRedirect updates a project's status then returns to the list.
func (s *Server) setStatusAndRedirect(w http.ResponseWriter, r *http.Request, status model.Status) {
	id := r.PathValue("id")
	if err := s.store.SetStatus(r.Context(), id, status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("set status", "id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/_app/projects", http.StatusSeeOther)
}

// handleProjectDelete removes the project row and its site directory.
func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteProject(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("delete project", "id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := s.fs.DeleteSite(id); err != nil {
		// The row is gone; log but do not fail the request.
		s.log.Error("delete site dir", "id", id, "error", err)
	}
	http.Redirect(w, r, "/_app/projects?flash=Project+deleted.", http.StatusSeeOther)
}

// loadProject fetches the project named by the {id} path value, writing a 404
// and returning ok=false when it does not exist.
func (s *Server) loadProject(w http.ResponseWriter, r *http.Request) (*model.Project, bool) {
	id := r.PathValue("id")
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return nil, false
		}
		s.log.Error("get project", "id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, false
	}
	return p, true
}

// filesURL builds the file-manager URL for a project with an optional raw query.
func (s *Server) filesURL(id, rawQuery string) string {
	u := "/_app/projects/" + id + "/files"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}

// humanBytes formats a byte count as a short human-readable string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
