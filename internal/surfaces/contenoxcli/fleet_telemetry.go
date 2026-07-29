package contenoxcli

import (
	"context"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/libtracker"
)

// newInstanceEventSink adapts the agentinstance lifecycle EventSink onto the
// shared ActivityTracker: every event is reported for audit and nothing
// else — no bus subject, no goroutine, no behavior triggered off it. The
// event carries only ids/state/kind, never prompt content, so it's safe to
// record whole; the synchronous Start/reportChange/end never blocks or calls
// back into the Manager.
func newInstanceEventSink(tracker libtracker.ActivityTracker) agentinstance.EventSink {
	return func(ev agentinstance.Event) {
		_, reportChange, end := tracker.Start(
			context.Background(),
			string(ev.Kind), "agent_instance",
			"instance_id", ev.InstanceID,
			"agent_id", ev.AgentID,
			"agent_name", ev.AgentName,
		)
		reportChange(ev.InstanceID, ev)
		end()
	}
}
