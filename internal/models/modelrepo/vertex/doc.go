// Package vertex implements the modelrepo.Provider contract against Google
// Vertex AI publisher endpoints, using OAuth bearer tokens minted from
// service-account credentials. The package registers its catalog at init
// time; depend on it via blank import where the catalog must be
// discoverable from runtimestate.
package vertex
