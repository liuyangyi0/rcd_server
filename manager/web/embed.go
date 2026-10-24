// Package webassets exposes the production web console for embedding in the
// single RCD Manager executable.
package webassets

import (
	"embed"
	"io/fs"
	"net/http"
)

// content is populated by `npm run build`. The checked-in placeholder keeps a
// clean checkout compilable, but a production build must contain index.html.
//
//go:embed dist/*
var content embed.FS

// FS returns the Vite production output with the dist prefix removed.
func FS() fs.FS {
	assets, err := fs.Sub(content, "dist")
	if err != nil {
		panic("webassets: cannot open embedded dist directory: " + err.Error())
	}
	return assets
}

// Handler serves the embedded console. The UI uses hash routing, so no
// history-API fallback is needed for navigation.
func Handler() http.Handler {
	return http.FileServer(http.FS(FS()))
}
