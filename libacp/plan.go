package libacp

import "encoding/json"

type PlanEntryPriority string

const (
	PlanPriorityHigh   PlanEntryPriority = "high"
	PlanPriorityMedium PlanEntryPriority = "medium"
	PlanPriorityLow    PlanEntryPriority = "low"
)

type PlanEntryStatus string

const (
	PlanStatusPending    PlanEntryStatus = "pending"
	PlanStatusInProgress PlanEntryStatus = "in_progress"
	PlanStatusCompleted  PlanEntryStatus = "completed"
)

type PlanEntry struct {
	Content  string            `json:"content"`
	Priority PlanEntryPriority `json:"priority"`
	Status   PlanEntryStatus   `json:"status"`
	Meta     json.RawMessage   `json:"_meta,omitempty"`
}

type AvailableCommandInput struct {
	Hint string `json:"hint,omitempty"`
}

type AvailableCommand struct {
	Name string `json:"name"`
	// Description is spec-required (strict clients reject commands without
	// it), so no omitempty: an empty string still reaches the wire.
	Description string                 `json:"description"`
	Input       *AvailableCommandInput `json:"input,omitempty"`
	Meta        json.RawMessage        `json:"_meta,omitempty"`
}
