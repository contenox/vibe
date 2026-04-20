// Package chatsessionmodes routes each chat turn: resolves which chain to run, injects context, executes steps, and persists the result.
package chatsessionmodes

import (
	"encoding/json"

	"github.com/contenox/contenox/runtime/taskengine"
)

// TurnInput is everything needed to run one user message through the chain engine.
type TurnInput struct {
	SessionID string
	Message   string
	// ExplicitChainRef is the chainId query param when set (wins over Mode).
	ExplicitChainRef string
	// SummarizerChainRef is required for build-mode turns (they compile a plan DAG
	// whose per-step terminals feed a summarizer subgraph). Ignored by other modes.
	SummarizerChainRef string
	Mode               string
	Context            *ContextPayload
	Model              string
	Provider           string
	RequestID          string
}

// ContextPayload mirrors the HTTP body; client artifacts become system messages.
type ContextPayload struct {
	Artifacts []ContextArtifact `json:"artifacts,omitempty"`
}

// ContextArtifact is one structured block merged into the thread before the user turn.
type ContextArtifact struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// TurnResult is the successful chat turn outcome (errors are returned as Go errors from SendTurn).
type TurnResult struct {
	Response         string
	State            []taskengine.CapturedStateUnit
	InputTokenCount  int
	OutputTokenCount int
}
