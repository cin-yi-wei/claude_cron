package channelagent

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// admin_dist is the built Svelte SPA (web/admin → vite build). It is committed
// to the repo so `go build` works without an npm toolchain. Rebuild with
// `cd web/admin && npm run build`.
//
//go:embed all:admin_dist
var adminDistFS embed.FS

// adminSPA is the http.Handler serving the Svelte SPA at /app/. It is the v2 UI
// and coexists with the interim Pico page at / (no regression). Static files are
// served unauthenticated (same rationale as /): a browser navigating here can't
// carry a bearer token; the page prompts for it and gates the /api/* calls.
//
// Built with vite base:'./', so asset URLs are relative and resolve under /app/.
// Unknown sub-paths fall back to index.html for client-side routing.
var adminSPA = func() http.Handler {
	sub, err := fs.Sub(adminDistFS, "admin_dist")
	if err != nil {
		panic("admin_spa: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip the /app prefix. Empty/unknown → "/" so FileServer returns
		// index.html WITHOUT a canonical /index.html→/ redirect (301).
		p := strings.TrimPrefix(r.URL.Path, "/app")
		p = strings.TrimPrefix(p, "/")
		r2 := r.Clone(r.Context())
		if p == "" || p == "index.html" {
			r2.URL.Path = "/"
		} else if _, err := fs.Stat(sub, p); err != nil {
			r2.URL.Path = "/" // not a real asset → SPA entry (client routing)
		} else {
			r2.URL.Path = "/" + p
		}
		fileServer.ServeHTTP(w, r2)
	})
}()
