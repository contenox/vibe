// Package llmrepo provides a unified facade over LLM backends discovered via
// runtimestate: prompt, chat, streaming, embedding, and tokenization through
// a single ModelRepo interface.
//
// External consumers can implement ModelRepo themselves or construct the
// default implementation with NewModelManager, which selects a concrete
// backend per call using model/provider hints and runtimestate's view of
// the available backends.
package llmrepo
