package contenoxcli

import (
	"context"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/libtracker"
)

// newInstanceEventSink adapts the agentinstance lifecycle EventSink onto the
// shared ActivityTracker: every Event (state change, attach, detach,
// unsupervised deny) is REPORTED through the tracker for after-the-fact audit
// and nothing else. It is PASSIVE per the fleet-manager telemetry ruling —
// record and return, no bus subject, no goroutine, no product behavior triggered
// off a lifecycle fact. With tracing off the tracker is a Noop, so the sink is a
// no-op too; it rides the tracing gate like every other subsystem.
//
// Payload discipline: the Event is identity-and-fact by construction (ids, agent
// name, state, kind — never prompt/output content), so it is safe to record
// whole. The EventSink contract forbids blocking or calling back into the
// Manager; a synchronous tracker.Start/reportChange/end obeys both.
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
