// Package ollama implements the modelrepo.Provider contract against Ollama
// HTTP endpoints. The package registers its catalog at init time; depend on
// it via blank import where the catalog must be discoverable from
// runtimestate.
package ollama
