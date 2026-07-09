// Package web embeds the HTML templates and static assets served by Paper
// Plane. The embed declarations live here, beside the assets, because go:embed
// patterns cannot traverse parent directories.
package web

import "embed"

// Templates holds all HTML templates under templates/.
//
//go:embed templates/*.html
var Templates embed.FS

// Static holds the admin UI's static assets (CSS + vendored htmx). Served under
// /_app/static/ so it never collides with a site slug (all non-/_app/* paths go
// to the slug resolver). The FS root contains the "static" directory, so
// requests are served as static/<path>.
//
//go:embed static
var Static embed.FS
