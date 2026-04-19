package serverapi

import (
	"net/http"

	"github.com/contenox/contenox/apiframework"
)

// AddVersionRoutes registers GET /version with JSON from apiframework.AboutServer.
// Handlers live here (not inside New) so tools/openapi-gen can discover the route via Add*Routes.
func AddVersionRoutes(mux *http.ServeMux, version, nodeInstanceID, tenancy string) {
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		_ = apiframework.Encode(w, r, http.StatusOK, apiframework.AboutServer{
			Version:        version,
			NodeInstanceID: nodeInstanceID,
			Tenancy:        tenancy,
		}) // @response apiframework.AboutServer
	})
}
