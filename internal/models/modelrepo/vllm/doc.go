// Package vllm implements the modelrepo.Provider contract against vLLM
// OpenAI-compatible HTTP endpoints. The package registers its catalog at
// init time; depend on it via blank import where the catalog must be
// discoverable from runtimestate.
package vllm
