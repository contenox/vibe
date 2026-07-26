package modelrepo

import "strings"

// CanonicalBackendType maps compatibility backend keywords to the implementation
// type used by the runtime by lowercasing and trimming the input.
func CanonicalBackendType(backendType string) string {
	return strings.ToLower(strings.TrimSpace(backendType))
}
