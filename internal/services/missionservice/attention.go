package missionservice

// AttentionAskedSubject is the bus subject published when a unit raises a
// question for a human — the mirror of ReportAddedSubject for "a unit needs
// something back" rather than "a unit said something". The ask itself is
// already durable in the approval store (hitlservice) the moment it is
// raised; this event only says it exists, so a subscriber can route it to
// the session that fired the mission rather than only the operator inbox.
const AttentionAskedSubject = "missionservice.events.attention_asked"

// AttentionAskedEvent is the self-contained event published when a unit
// raises a question. AskID is the durable ask's id (hitlservice.Answer,
// POST /api/approvals/{id}), so a subscriber can answer it without a second
// lookup.
type AttentionAskedEvent struct {
	MissionID       string `json:"missionId"`
	AskID           string `json:"askId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	Summary         string `json:"summary"`
	Detail          string `json:"detail,omitempty"`
}
