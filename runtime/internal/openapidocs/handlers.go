package openapidocs

import "net/http"

// Register adds GET /openapi.json (raw spec) and GET /docs (RapiDoc UI) on mux.
// Register these on the root mux before the SPA catch-all so paths are not shadowed.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(specJSON)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(rapidocHTML)
	})
}
