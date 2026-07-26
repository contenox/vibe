package runtimestate

import (
	_ "github.com/contenox/beam/internal/models/modelrepo/anthropic"
	_ "github.com/contenox/beam/internal/models/modelrepo/bedrock"
	_ "github.com/contenox/beam/internal/models/modelrepo/gemini"
	_ "github.com/contenox/beam/internal/models/modelrepo/ollama"
	_ "github.com/contenox/beam/internal/models/modelrepo/openai"
	_ "github.com/contenox/beam/internal/models/modelrepo/vertex"
	_ "github.com/contenox/beam/internal/models/modelrepo/vllm"
)

// Import vendor catalog providers for registry-based modelrepo catalog construction.
