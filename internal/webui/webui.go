// Package webui serves the embedded admin console. It is a static
// single-page app that talks to the admin API with the admin token the user
// enters; the server holds no session state.
package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"strings"
)

func init() {
	// Not in Go's built-in table; minimal images have no /etc/mime.types.
	mime.AddExtensionType(".woff2", "font/woff2")
}

//go:embed static
var files embed.FS

// Handler serves the console under prefix (e.g. "/ui/").
func Handler(prefix string) http.Handler {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.StripPrefix(prefix, http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, prefix+"fonts/") {
			h.Set("Cache-Control", "public, max-age=604800") // fonts never change in place
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
