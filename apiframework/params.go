package apiframework

import (
	"net/http"
)

// GetPathParam retrieves a URL path parameter by name and is used to enforce
// that all path parameters are documented for the OpenAPI generator.
// The description argument is not used at runtime but is required for API spec generation.
func GetPathParam(r *http.Request, name string, description string) string {
	// The description parameter exists solely for the static analysis tool.
	// It has no effect on the runtime execution of this function.
	return r.PathValue(name)
}

// GetQueryParam retrieves a URL query parameter by name. If the parameter is not
// present, it returns the provided defaultValue. The description is used for OpenAPI generation.
func GetQueryParam(r *http.Request, name, defaultValue, description string) string {
	val := r.URL.Query().Get(name)
	if val == "" {
		return defaultValue
	}
	return val
}
