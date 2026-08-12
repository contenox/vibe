package missionservice

// AttentionAskedSubject is the bus subject published when a unit raises a question for a human — the ask itself is already durable in the approval store the moment it is raised, so this event only says it exists, letting a subscriber route it beyond the operator inbox.
const AttentionAskedSubject = "missionservice.events.attention_asked"

// AttentionAskedEvent is the self-contained event published when a unit raises a question; AskID is the durable ask's id (hitlservice.Answer, POST /api/approvals/{id}), letting a subscriber answer it without a second lookup.
type AttentionAskedEvent struct {
	MissionID       string `json:"missionId"`
	AskID           string `json:"askId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	Summary         string `json:"summary"`
	Detail          string `json:"detail,omitempty"`
}
