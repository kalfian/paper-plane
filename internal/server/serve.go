package server

import (
	"bytes"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/kalfian/paper-plane/internal/model"
	"github.com/kalfian/paper-plane/internal/store"
)

// handleServeSite is the fallback handler for every request that is not under
// /_app/*. It resolves the first path segment as a project slug and serves the
// project's static files. Unknown or unlinked slugs return 404. It is
// registered on the root pattern ("GET /"), which the more specific /_app/*
// patterns take precedence over.
func (s *Server) handleServeSite(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	slug, rest, hadSlash := strings.Cut(trimmed, "/")

	if slug == "" {
		// Bare "/" → send visitors to the admin app.
		http.Redirect(w, r, "/_app/", http.StatusFound)
		return
	}

	proj, err := s.store.GetProjectBySlug(r.Context(), slug)
	if err != nil || proj.Status != model.StatusActive {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("lookup slug", "slug", slug, "error", err)
		}
		http.NotFound(w, r)
		return
	}

	// Redirect "/<slug>" (no trailing slash) → "/<slug>/" so relative asset
	// paths and the injected <base> resolve correctly.
	if !hadSlash {
		target := "/" + slug + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}

	s.serveSiteFile(w, r, proj, rest)
}

// serveSiteFile serves relpath from a project's site directory, applying the
// default index, directory-trailing-slash redirect, content type, cache
// headers, and (for the root index.html) <base> injection.
func (s *Server) serveSiteFile(w http.ResponseWriter, r *http.Request, proj *model.Project, relpath string) {
	// The site root's directory index is the project's configured landing page
	// (default index.html); subdirectories still use index.html.
	rootRequest := relpath == "" || relpath == "."

	info, err := s.fs.Stat(proj.ID, relpath)
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}

	if info.IsDir() {
		// Ensure directory URLs end with "/" before serving their index.
		if !strings.HasSuffix(r.URL.Path, "/") {
			target := r.URL.Path + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		indexName := "index.html"
		if rootRequest {
			indexName = proj.EffectiveIndexFile()
		}
		relpath = path.Join(relpath, indexName)
		info, err = s.fs.Stat(proj.ID, relpath)
		if err != nil {
			s.notFoundOrError(w, r, err)
			return
		}
		if info.IsDir() {
			http.NotFound(w, r)
			return
		}
	}

	data, err := s.fs.ReadFile(proj.ID, relpath)
	if err != nil {
		s.notFoundOrError(w, r, err)
		return
	}

	// Inject <base> into the site's root landing page (the configured index file
	// served at the root, whatever its name). Root-level files only: a nested
	// path (e.g. "sub/index.html") is never the site's landing page.
	if relpath == proj.EffectiveIndexFile() && !strings.Contains(relpath, "/") {
		data = injectBase(data, proj.Slug)
	}

	ext := path.Ext(relpath)
	if ctype := mime.TypeByExtension(ext); ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.Header().Set("Cache-Control", cacheControlFor(ext))

	http.ServeContent(w, r, path.Base(relpath), info.ModTime(), bytes.NewReader(data))
}

// notFoundOrError maps a filesystem error to 404 (missing) or 500 (unexpected).
func (s *Server) notFoundOrError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	s.log.Error("serve site file", "path", r.URL.Path, "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// cacheControlFor returns a reasonable Cache-Control value by extension. HTML is
// revalidated each time (content changes on edit); other assets get a short TTL.
func cacheControlFor(ext string) string {
	switch strings.ToLower(ext) {
	case ".html", ".htm", "":
		return "no-cache"
	default:
		return "public, max-age=3600"
	}
}

// injectBase inserts <base href="/<slug>/"> immediately after the opening
// <head> tag, best-effort:
//   - skips injection if the document already has a <base> tag;
//   - skips if no <head> tag is found.
//
// slug is validated to match ^[a-z0-9][a-z0-9-]*$, so it contains no characters
// that need HTML escaping inside the attribute value.
func injectBase(doc []byte, slug string) []byte {
	lower := bytes.ToLower(doc)
	if bytes.Contains(lower, []byte("<base")) {
		return doc
	}
	head := bytes.Index(lower, []byte("<head"))
	if head < 0 {
		return doc
	}
	// Find the end ('>') of the opening <head ...> tag.
	gt := bytes.IndexByte(lower[head:], '>')
	if gt < 0 {
		return doc
	}
	insertAt := head + gt + 1

	tag := []byte(`<base href="/` + slug + `/">`)
	out := make([]byte, 0, len(doc)+len(tag))
	out = append(out, doc[:insertAt]...)
	out = append(out, tag...)
	out = append(out, doc[insertAt:]...)
	return out
}
