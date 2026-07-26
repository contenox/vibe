// Package openai implements the modelrepo.Provider contract against the
// OpenAI HTTP API and OpenAI-compatible endpoints. The package registers
// its catalog at init time; depend on it via blank import where the catalog
// must be discoverable from runtimestate.
package openai
