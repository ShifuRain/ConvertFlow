// Package ui serves the embedded web UI.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	// The embedded directory carries a .gitkeep placeholder; dotfiles are never served.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, seg := range strings.Split(r.URL.Path, "/") {
			if strings.HasPrefix(seg, ".") {
				http.NotFound(w, r)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
