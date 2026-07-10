package server

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"

	"github.com/kalfian/paper-plane/web"
)

// TEMPLATE CONTRACT
// -----------------
// Templates are embedded from web/templates/*.html. The full data contract each
// handler passes to each template is documented in web/templates/CONTRACT.md —
// keep that file and the handlers in sync.
//
// LAYOUT INHERITANCE (clone-per-page)
// -----------------------------------
// The naive approach — parsing every file into ONE html/template set and
// executing by filename — cannot support a shared layout: each page would have
// to `{{define "content"}}`, and those definitions collide inside a single set
// (last one wins). Instead we build one independent template set PER PAGE:
//
//	base := layout.html
//	page set = base.Clone() + parse(page.html)
//
// Every content page (projects/project_new/project_edit/files) defines a
// `{{define "content"}}` block; the cloned layout's `{{block "content" .}}`
// picks up that page's definition without any cross-page collision. We then
// execute the layout ("layout.html") as the entry template.
//
// login.html is standalone (it renders before authentication and needs no admin
// chrome), so it is parsed on its own and executed directly by filename.
//
// The public API — render(w, status, "<file>.html", data) — is unchanged; the
// renderer maps the requested filename to its prepared set and entry template.

// pageTemplate pairs a prepared template set with the entry template to execute.
type pageTemplate struct {
	set   *template.Template
	entry string // template name to execute (e.g. "layout.html" or "login.html")
}

// renderer holds one prepared template set per page.
type renderer struct {
	pages map[string]*pageTemplate
}

// layoutPages are content pages that inherit the shared layout shell.
var layoutPages = []string{
	"projects.html",
	"project_new.html",
	"project_edit.html",
	"files.html",
	"settings.html",
}

// standalonePages render their own full document without the layout shell.
// login and setup both render before authentication, so they carry no admin
// chrome.
var standalonePages = []string{"login.html", "setup.html"}

// funcMap holds template helpers available to every page.
var funcMap = template.FuncMap{
	// pageTitle joins a page-specific title with the app name for <title>.
	"pageTitle": func(s string) string {
		if s == "" {
			return "Paper Plane"
		}
		return s + " · Paper Plane"
	},
}

// newRenderer builds an independent template set per page (clone-per-page layout
// inheritance). It returns an error if any template fails to parse, surfacing
// problems at startup rather than at request time.
func newRenderer() (*renderer, error) {
	base, err := template.New("layout.html").Funcs(funcMap).ParseFS(web.Templates, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("parse layout: %w", err)
	}

	pages := make(map[string]*pageTemplate, len(layoutPages)+len(standalonePages))

	for _, name := range layoutPages {
		clone, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone layout for %s: %w", name, err)
		}
		if _, err := clone.ParseFS(web.Templates, "templates/"+name); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		pages[name] = &pageTemplate{set: clone, entry: "layout.html"}
	}

	for _, name := range standalonePages {
		set, err := template.New(name).Funcs(funcMap).ParseFS(web.Templates, "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		pages[name] = &pageTemplate{set: set, entry: name}
	}

	return &renderer{pages: pages}, nil
}

// render executes the page registered under name (by filename, e.g.
// "login.html") into a buffer first so that a template execution error results
// in a clean 500 rather than a half-written response. status is the HTTP status
// on success.
func (rd *renderer) render(w http.ResponseWriter, status int, name string, data any) {
	pt, ok := rd.pages[name]
	if !ok {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := pt.set.ExecuteTemplate(&buf, pt.entry, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// loginData is the data passed to login.html. See web/templates/CONTRACT.md.
type loginData struct {
	CSRFToken string
	Error     string
}

// setupData is the data passed to setup.html (first-run password setup). See
// web/templates/CONTRACT.md.
type setupData struct {
	CSRFToken string
	Error     string
}
