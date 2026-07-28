// Package modelrepo defines the provider-facing contracts for LLM backends:
// the Provider interface, per-capability client interfaces, and shared
// request/response types. Concrete providers live in subpackages (openai,
// gemini, vertex, vllm, ollama, anthropic, bedrock); higher-level code such as
// llmrepo and runtimestate depends only on the interfaces declared here.
package modelrepo
