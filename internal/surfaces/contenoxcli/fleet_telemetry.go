package contenoxcli

import (
	"context"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/libtracker"
)

// newInstanceEventSink adapts the agentinstance lifecycle EventSink onto the
// shared ActivityTracker, reporting every event for audit and nothing else.
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
