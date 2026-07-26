package acpsvc

import (
	"encoding/json"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/libacp"
)

// planToolEventName is the fully-qualified tool name a mission_plan call carries
// on its task event. Both call paths namespace the leaf as "<provider>.<tool>"
// (the model path uses toolCall.Function.Name; the deterministic `tools` path
// joins Tools.Name and Tools.ToolName), so the projection matches on that one
// exact string rather than on a suffix that a differently-named provider could
// collide with.
const planToolEventName = missiontools.ToolsProviderName + "." + missiontools.ToolNamePlan

// planUpdateNotification is the ACP projection of the plan engine: when a unit's
// mission_plan tool call succeeds, its task event carries the stored plan
// snapshot as JSON (missiontools.execPlan returns the mission's Plan verbatim),
// and this turns that snapshot into a full-snapshot `plan` session update — the
// one wire surface ACP defines for plans (agent→client, whole list each time,
// render-only; see the mission-plans blueprint's "ACP boundary").
//
// It is the idiomatic seam for two reasons. First, it lives in the SAME
// event-translation layer that already turns every other tool event into a
// session update (events.go), so the plan projection reaches the unit's stream
// through exactly the path tool activity always has — and only the OWNING unit's
// session, because the event subscription that feeds translateEvents is scoped to
// that session's own turn (prompt.go's per-request bus.Stream). Second, it reads
// the tool event alone and reaches into nothing: missionservice stays a store,
// missiontools stays a tool grant, and neither is coupled to this transport. The
// echo the planner needs (ids carried forward) and the snapshot this projection
// needs are one and the same JSON — one carrier, two consumers.
//
// The cast from missionservice's plan enums to libacp's is a plain string
// conversion, safe because the two enum sets are contracted byte-equal — pinned
// by TestUnit_PlanProjection_EnumParity in this package, the parity test Slice 1
// promised would live in the projection slice.
//
// Returns (_, false) — emit nothing — when the event is not a successful
// mission_plan call, or when its content does not parse as a plan snapshot. That
// last case degrades gracefully by design: the durable plan is unaffected (it was
// already stored before the event fired), a stale or capped echo simply skips one
// projected update rather than emitting a corrupt one, and the next revision
// re-projects the full snapshot.
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
	// A successful SetPlan never stores an empty plan (it rejects the degenerate
	// no-entries snapshot), so a parse yielding zero entries is not a real plan
	// revision — most likely a non-plan JSON that happened to unmarshal cleanly.
	// Skip it rather than emit an empty `plan` update.
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
