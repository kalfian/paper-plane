package server

import (
	"errors"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/sitefs"
)

// uploadParseMemory is the in-memory buffer size for multipart parsing; larger
// parts spill to temporary files on disk.
const uploadParseMemory = 8 << 20 // 8 MiB

// maxRequestBody caps the total request body for uploads, slightly above the
// site file-size budget to allow for multipart overhead.
const maxRequestBody = sitefs.DefaultMaxUploadBytes + (1 << 20)

// textEditableExts are the file extensions editable via the in-app text editor.
var textEditableExts = map[string]bool{
	".html": true, ".htm": true, ".css": true, ".js": true,
	".txt": true, ".md": true, ".json": true, ".svg": true,
	".xml": true, ".csv": true,
}

// isTextEditable reports whether relpath has an editable text extension.
func isTextEditable(relpath string) bool {
	return textEditableExts[strings.ToLower(path.Ext(relpath))]
}

// --- template data (see web/templates/CONTRACT.md) ---

// fileView is a single row in the file manager.
type fileView struct {
	Path      string
	Size      int64
	SizeHuman string
	Editable  bool
	IsHTML    bool // eligible to be set as the landing page
	IsIndex   bool // true when this file is the project's landing page
}

// filesData is passed to files.html.
type filesData struct {
	CSRFToken string
	Project   projectView
	Files     []fileView
	// Editor fields: populated when viewing/editing a single text file.
	Editing     bool
	EditPath    string
	EditContent string
	// Rename fields: populated when renaming a single file.
	Renaming   bool
	RenamePath string
	Flash      string
	Error      string
}

// handleFilesList renders the file manager for a project.
func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	data := s.filesDataFor(p.ID, *p)
	data.Flash = r.URL.Query().Get("flash")
	data.Error = r.URL.Query().Get("error")
	s.rd.render(w, http.StatusOK, "files.html", data)
}

// handleFileEdit renders the file manager with a text file loaded into the
// editor. The path is taken from the ?path= query.
func (s *Server) handleFileEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	relpath := r.URL.Query().Get("path")
	data := s.filesDataFor(p.ID, *p)
	data.Flash = r.URL.Query().Get("flash")
	data.Error = r.URL.Query().Get("error")

	if !isTextEditable(relpath) {
		data.Error = "That file type cannot be edited in the browser."
		s.rd.render(w, http.StatusOK, "files.html", data)
		return
	}
	content, err := s.fs.ReadFile(p.ID, relpath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, sitefs.ErrUnsafePath) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("read file for edit", "id", p.ID, "path", relpath, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data.Editing = true
	data.EditPath = relpath
	data.EditContent = string(content)
	s.rd.render(w, http.StatusOK, "files.html", data)
}

// handleFileSave writes edited text content back to a file. Only text-editable
// extensions are allowed.
func (s *Server) handleFileSave(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	relpath := strings.TrimSpace(r.FormValue("path"))
	content := r.FormValue("content")

	if !isTextEditable(relpath) {
		http.Redirect(w, r, s.filesURL(p.ID, "error=That+file+type+cannot+be+edited."), http.StatusSeeOther)
		return
	}
	if err := s.fs.WriteFile(p.ID, relpath, []byte(content)); err != nil {
		if errors.Is(err, sitefs.ErrUnsafePath) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		s.log.Error("save file", "id", p.ID, "path", relpath, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Keep the editor open on the same file, with a success flash. This is a
	// POST-redirect-GET to the edit view; htmx follows it and re-swaps #app-main,
	// so the textarea stays populated and focus is restored (see layout.html).
	http.Redirect(w, r, s.fileEditURL(p.ID, relpath, "File saved."), http.StatusSeeOther)
}

// fileEditURL builds the editor URL for a project file, escaping the relative
// path and attaching an optional flash message.
func (s *Server) fileEditURL(id, relpath, flash string) string {
	q := url.Values{}
	q.Set("path", relpath)
	if flash != "" {
		q.Set("flash", flash)
	}
	return "/_app/projects/" + id + "/files/edit?" + q.Encode()
}

// handleFileDelete removes a single file from a project's site.
func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	relpath := strings.TrimSpace(r.FormValue("path"))
	if err := s.fs.DeleteFile(p.ID, relpath); err != nil {
		if errors.Is(err, sitefs.ErrUnsafePath) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if errors.Is(err, fs.ErrNotExist) {
			http.Redirect(w, r, s.filesURL(p.ID, "error=File+not+found."), http.StatusSeeOther)
			return
		}
		s.log.Error("delete file", "id", p.ID, "path", relpath, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.filesURL(p.ID, "flash=File+deleted."), http.StatusSeeOther)
}

// filenameRe matches an acceptable single-segment filename: no path separators,
// not "." or "..", printable, reasonable length. Renames keep files at the site
// root (the manager is flat), so a bare filename is all we accept.
var filenameRe = regexp.MustCompile(`^[^/\\\x00]{1,255}$`)

// validFilename reports a friendly error ("" when valid) for a rename target.
func validFilename(name string) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "A file name is required."
	case name == "." || name == "..":
		return "That file name is not allowed."
	case !filenameRe.MatchString(name):
		return "File name must not contain slashes or control characters."
	default:
		return ""
	}
}

// handleFileRenameForm renders the file manager with a rename form for one file.
// The path comes from the ?path= query.
func (s *Server) handleFileRenameForm(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	relpath := r.URL.Query().Get("path")
	data := s.filesDataFor(p.ID, *p)
	if !s.fileExists(p.ID, relpath) {
		http.NotFound(w, r)
		return
	}
	data.Renaming = true
	data.RenamePath = relpath
	s.rd.render(w, http.StatusOK, "files.html", data)
}

// handleFileRename renames a file within the site. When the renamed file was the
// project's landing page, the project's index_file follows the new name.
func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	oldPath := strings.TrimSpace(r.FormValue("path"))
	newName := strings.TrimSpace(r.FormValue("name"))
	if msg := validFilename(newName); msg != "" {
		http.Redirect(w, r, s.filesURL(p.ID, "error="+url.QueryEscape(msg)), http.StatusSeeOther)
		return
	}
	// Keep the file in its current directory; only the base name changes.
	newPath := path.Join(path.Dir(oldPath), newName)

	if err := s.fs.Rename(p.ID, oldPath, newPath); err != nil {
		switch {
		case errors.Is(err, sitefs.ErrUnsafePath):
			http.Error(w, "invalid path", http.StatusBadRequest)
		case errors.Is(err, fs.ErrNotExist):
			http.Redirect(w, r, s.filesURL(p.ID, "error=File+not+found."), http.StatusSeeOther)
		case errors.Is(err, fs.ErrExist):
			http.Redirect(w, r, s.filesURL(p.ID, "error=A+file+with+that+name+already+exists."), http.StatusSeeOther)
		default:
			s.log.Error("rename file", "id", p.ID, "old", oldPath, "new", newPath, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	// If the landing page was renamed, keep the index pointer in sync.
	if oldPath == p.EffectiveIndexFile() {
		if err := s.store.SetIndexFile(r.Context(), p.ID, newPath); err != nil {
			s.log.Error("update index after rename", "id", p.ID, "error", err)
		}
	}
	http.Redirect(w, r, s.filesURL(p.ID, "flash=File+renamed."), http.StatusSeeOther)
}

// handleFileSetIndex points the project's landing page at the given file.
func (s *Server) handleFileSetIndex(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	relpath := strings.TrimSpace(r.FormValue("path"))
	if !s.fileExists(p.ID, relpath) {
		http.Redirect(w, r, s.filesURL(p.ID, "error=File+not+found."), http.StatusSeeOther)
		return
	}
	if err := s.store.SetIndexFile(r.Context(), p.ID, relpath); err != nil {
		s.log.Error("set index file", "id", p.ID, "path", relpath, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.filesURL(p.ID, "flash=Landing+page+updated."), http.StatusSeeOther)
}

// handleFilesUpload accepts single, multiple, or zip uploads and stores them.
func (s *Server) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := r.ParseMultipartForm(uploadParseMemory); err != nil {
		http.Redirect(w, r, s.filesURL(p.ID, "error=Could+not+read+the+upload."), http.StatusSeeOther)
		return
	}
	// Whether the site already has a landing page decides if a lone HTML upload
	// should auto-become the index (checked before writing the new files).
	hadIndex := s.fileExists(p.ID, p.EffectiveIndexFile())

	added, err := s.processUploads(p.ID, r)
	if err != nil {
		http.Redirect(w, r, s.filesURL(p.ID, "error="+uploadErrorMessage(err)), http.StatusSeeOther)
		return
	}
	if added == 0 {
		http.Redirect(w, r, s.filesURL(p.ID, "error=No+files+were+uploaded."), http.StatusSeeOther)
		return
	}
	// A lone HTML page uploaded into a site with no landing page yet becomes the
	// index — kept under its own name (no rename).
	if !hadIndex {
		if name := loneHTMLUpload(r); name != "" {
			if err := s.store.SetIndexFile(r.Context(), p.ID, name); err != nil {
				s.log.Error("set index file", "id", p.ID, "name", name, "error", err)
			}
		}
	}
	http.Redirect(w, r, s.filesURL(p.ID, "flash=Uploaded."), http.StatusSeeOther)
}

// processUploads handles the multipart "files" field: zip files are extracted,
// everything else is stored under its base filename at the site root. It
// returns the number of files added. The caller must have parsed the multipart
// form. It returns nil error when there are simply no files. Files keep their
// original names; selecting a landing page is the caller's job (see
// loneHTMLUpload / store.SetIndexFile).
func (s *Server) processUploads(id string, r *http.Request) (int, error) {
	if r.MultipartForm == nil {
		return 0, nil
	}
	var added int
	for _, fh := range r.MultipartForm.File["files"] {
		if fh == nil || fh.Filename == "" {
			continue
		}
		f, err := fh.Open()
		if err != nil {
			return added, err
		}
		if strings.EqualFold(filepath.Ext(fh.Filename), ".zip") {
			n, zerr := s.fs.ExtractZip(id, f, fh.Size)
			_ = f.Close()
			added += n
			if zerr != nil {
				return added, zerr
			}
			continue
		}
		data, rerr := io.ReadAll(f)
		_ = f.Close()
		if rerr != nil {
			return added, rerr
		}
		// Strip any client-supplied directory; single/multi uploads land at the
		// site root under their base name. Zip is the way to preserve structure.
		name := filepath.Base(fh.Filename)
		if err := s.fs.WriteFile(id, name, data); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

// loneHTMLUpload returns the base filename when the request carries exactly one
// non-index HTML file (and nothing else), else "". A lone uploaded page is a
// natural candidate for the site's landing page — the caller may point the
// project's index_file at it without renaming the file.
func loneHTMLUpload(r *http.Request) string {
	if r.MultipartForm == nil {
		return ""
	}
	var files []*multipart.FileHeader
	for _, fh := range r.MultipartForm.File["files"] {
		if fh != nil && fh.Filename != "" {
			files = append(files, fh)
		}
	}
	if len(files) != 1 {
		return ""
	}
	name := filepath.Base(files[0].Filename)
	if !isHTMLName(name) || strings.EqualFold(name, "index.html") {
		return ""
	}
	return name
}

// isHTMLName reports whether name has an .html or .htm extension.
func isHTMLName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm":
		return true
	default:
		return false
	}
}

// fileExists reports whether relpath exists within the site.
func (s *Server) fileExists(id, relpath string) bool {
	_, err := s.fs.Stat(id, relpath)
	return err == nil
}

// filesDataFor builds the base filesData (project + file list) for rendering.
func (s *Server) filesDataFor(id string, p model.Project) filesData {
	files, err := s.fs.ListFiles(id)
	if err != nil {
		s.log.Error("list files", "id", id, "error", err)
	}
	index := p.EffectiveIndexFile()
	views := make([]fileView, 0, len(files))
	for _, f := range files {
		views = append(views, fileView{
			Path:      f.Path,
			Size:      f.Size,
			SizeHuman: humanBytes(f.Size),
			Editable:  isTextEditable(f.Path),
			IsHTML:    isHTMLName(f.Path),
			IsIndex:   f.Path == index,
		})
	}
	return filesData{
		CSRFToken: s.auth.IssueCSRFToken(),
		Project:   s.newProjectView(p),
		Files:     views,
	}
}

// uploadErrorMessage maps a sitefs upload error to a URL-safe, friendly message.
func uploadErrorMessage(err error) string {
	switch {
	case errors.Is(err, sitefs.ErrTooLarge):
		return "Upload+exceeds+the+size+limit."
	case errors.Is(err, sitefs.ErrTooManyEntries):
		return "Zip+has+too+many+entries."
	case errors.Is(err, sitefs.ErrUnsafeEntry), errors.Is(err, sitefs.ErrUnsafePath):
		return "Upload+contained+an+unsafe+path."
	default:
		return "Upload+failed."
	}
}
