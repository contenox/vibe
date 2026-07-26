package missionservice

// AttentionAskedSubject is the bus subject a raised ATTENTION ASK is published
// on — a unit's QUESTION for a human, the mirror of ReportAddedSubject's "a unit
// said something" for "a unit needs something back".
//
// It exists for the same reason and follows the same seam: the ask itself is
// durable in the approval store the moment it is raised (hitlservice), and this
// event only says it EXISTS so a subscriber can decide where a human should be
// told. Reports already travel this way to whoever fired the mission; a question
// that only reached the operator inbox — while the session that fired the
// mission sat there unaware — was the asymmetry this closes.
const AttentionAskedSubject = "missionservice.events.attention_asked"

// AttentionAskedEvent is the SELF-CONTAINED domain event published when a unit
// raises a question. Self-contained like ReportAddedEvent: a subscriber routes it
// without reading anything back.
//
// AskID is the durable ask's id — the handle an answer is given against
// (hitlservice.Answer, POST /api/approvals/{id}) — so a surface that renders this
// event can answer it without a second lookup.
type AttentionAskedEvent struct {
	MissionID       string `json:"missionId"`
	AskID           string `json:"askId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	Summary         string `json:"summary"`
	Detail          string `json:"detail,omitempty"`
}
