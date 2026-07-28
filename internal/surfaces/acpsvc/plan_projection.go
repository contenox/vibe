package acpsvc

import (
	"encoding/json"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/libacp"
)

// planToolEventName is the fully-qualified tool name a mission_plan call
// carries on its task event ("<provider>.<tool>"); matched as an exact string
// so a differently-named provider can't collide via a suffix.
const planToolEventName = missiontools.ToolsProviderName + "." + missiontools.ToolNamePlan

// planUpdateNotification projects a successful mission_plan tool event's
// stored plan snapshot into a full-snapshot ACP `plan` session update, in
// addition to the tool-call card events.go already emits for every tool event.
//
// The enum cast from missionservice to libacp is a safe plain string
// conversion: the two enum sets are contracted byte-equal, pinned by
// TestUnit_PlanProjection_EnumParity.
//
// Returns (_, false) — emit nothing — when the event is not a successful,
// parseable mission_plan call. The durable plan is unaffected either way; the
// next revision re-projects the full snapshot.
func planUpdateNotification(sid libacp.SessionID, ev taskengine.TaskEvent) (libacp.SessionNotification, bool) {
	if ev.ToolName != planToolEventName || ev.Error != "" {
		return libacp.SessionNotification{}, false
	}
	if ev.Content == "" {
		return libacp.SessionNotification{}, false
	}
	var plan missionservice.Plan
	if err := json.Unmarshal([]byte(ev.Content), &plan); err != nil {
		return libacp.SessionNotification{}, false
	}
	// SetPlan rejects an empty plan, so zero entries here means the content
	// wasn't really a plan revision — skip rather than emit an empty update.
	if len(plan.Entries) == 0 {
		return libacp.SessionNotification{}, false
	}
	entries := make([]libacp.PlanEntry, 0, len(plan.Entries))
	for _, e := range plan.Entries {
		entries = append(entries, libacp.PlanEntry{
			Content:  e.Content,
			Priority: libacp.PlanEntryPriority(e.Priority),
			Status:   libacp.PlanEntryStatus(e.Status),
		})
	}
	return libacp.SessionNotification{
		SessionID: sid,
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdatePlan,
			Entries:       entries,
		},
	}, true
}
