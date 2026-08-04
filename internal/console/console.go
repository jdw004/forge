// Package console embeds a dependency-free operations console in the Go binary.
package console

import (
	"embed"
	"net/http"
)

//go:embed assets/index.html assets/console.css assets/console.mjs assets/model.mjs
var assets embed.FS

// Handler serves only the known public shell assets. Workflow data continues
// to require API authentication; arbitrary filesystem paths are never served.
func Handler() http.Handler {
	files := map[string]struct{ name, contentType string }{
		"/":                   {"index.html", "text/html; charset=utf-8"},
		"/assets/console.css": {"console.css", "text/css; charset=utf-8"},
		"/assets/console.mjs": {"console.mjs", "text/javascript; charset=utf-8"},
		"/assets/model.mjs":   {"model.mjs", "text/javascript; charset=utf-8"},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := assets.ReadFile("assets/" + file.name)
		if err != nil {
			http.Error(w, "Console unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", file.contentType)
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
}
