package runtimestate

import (
	_ "github.com/contenox/contenox/runtime/internal/modelrepo/gemini"
	_ "github.com/contenox/contenox/runtime/internal/modelrepo/local"
	_ "github.com/contenox/contenox/runtime/internal/modelrepo/ollama"
	_ "github.com/contenox/contenox/runtime/internal/modelrepo/openai"
	_ "github.com/contenox/contenox/runtime/internal/modelrepo/vertex"
	_ "github.com/contenox/contenox/runtime/internal/modelrepo/vllm"
)

// Import vendor catalog providers for registry-based modelrepo catalog construction.
