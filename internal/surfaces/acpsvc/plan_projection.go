package acpsvc

import (
	"encoding/json"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/libacp"
)

// planToolEventName is the fully-qualified tool name a mission_plan call carries
// on its task event.
const planToolEventName = missiontools.ToolsProviderName + "." + missiontools.ToolNamePlan

// planUpdateNotification projects a successful mission_plan tool event's stored
// plan snapshot into a full-snapshot ACP plan session update. The enum cast to
// libacp is byte-equal by contract, pinned by TestUnit_PlanProjection_EnumParity.
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
	// SetPlan rejects an empty plan, so zero entries means this wasn't one.
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
