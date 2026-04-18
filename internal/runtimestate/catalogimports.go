package runtimestate

import (
	_ "github.com/contenox/contenox/internal/modelrepo/gemini"
	_ "github.com/contenox/contenox/internal/modelrepo/local"
	_ "github.com/contenox/contenox/internal/modelrepo/ollama"
	_ "github.com/contenox/contenox/internal/modelrepo/openai"
	_ "github.com/contenox/contenox/internal/modelrepo/vertex"
	_ "github.com/contenox/contenox/internal/modelrepo/vllm"
)

// Import vendor catalog providers for registry-based modelrepo catalog construction.
