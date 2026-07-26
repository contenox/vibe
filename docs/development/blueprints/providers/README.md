# Provider Blueprints

Cloud/hosted model providers plug into `internal/models/modelrepo` behind the
`modelrepo.Provider` interface; request-side selection happens in
`internal/models/llmrepo`. These docs cover provider-specific integration designs.

| Doc | Status | What it covers |
| --- | --- | --- |
| [aws-bedrock.md](aws-bedrock.md) | implemented | Bedrock Converse API provider: dependency assessment, auth chain, codec fit |
