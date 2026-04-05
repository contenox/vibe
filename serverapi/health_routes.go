package serverapi

import "net/http"

// AddHealthRoutes registers GET /health for liveness checks.
func AddHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}
