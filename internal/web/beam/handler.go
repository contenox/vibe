package beam

import (
	"io/fs"
	"net/http"
	"strings"
)

// Handler serves the embedded Beam React SPA with proper fallback for client-side routing
func Handler() http.Handler {
	subFS, err := fs.Sub(Dist, "dist")
	if err != nil {
		panic("failed to create sub FS for beam: " + err.Error())
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Let API routes be handled by your API router (register /api/* first!)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}

		// SPA fallback: if file doesn't exist, serve index.html
		if r.URL.Path != "/" && r.URL.Path != "" {
			_, err := subFS.Open(strings.TrimLeft(r.URL.Path, "/"))
			if err != nil {
				r.URL.Path = "/"
			}
		}

		http.FileServer(http.FS(subFS)).ServeHTTP(w, r)
	})
}
