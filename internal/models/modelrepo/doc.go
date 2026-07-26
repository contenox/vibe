// Package modelrepo defines the provider-facing contracts for LLM backends:
// the Provider interface (capabilities + client factories), the per-capability
// client interfaces (LLMPromptExecClient, LLMChatClient, LLMEmbedClient,
// LLMStreamClient), and the shared request/response types (Message,
// ChatResult, StreamParcel, Tool, ChatArgument).
//
// Concrete providers live in subpackages (openai, gemini, vertex, vllm,
// ollama, anthropic, mistral, openrouter, bedrock). Higher-level code such as llmrepo and runtimestate depends
// only on the interfaces declared here; provider subpackages are imported
// for their side effects to register catalogs with runtimestate.
package modelrepo
