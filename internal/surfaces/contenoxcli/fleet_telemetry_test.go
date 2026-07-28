package contenoxcli

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// capturedStart records one tracker.Start span and the reportChange it received,
// so the adapter test can assert the op/subject/fields a lifecycle Event maps to.
type capturedStart struct {
	op       string
	subject  string
	kv       []any
	changeID string
	change   any
}

type captureTracker struct {
	starts []capturedStart
}

func (c *captureTracker) Start(_ context.Context, op, subject string, kv ...any) (
	func(error), func(string, any), func(),
) {
	idx := len(c.starts)
	c.starts = append(c.starts, capturedStart{op: op, subject: subject, kv: kv})
	reportChange := func(id string, data any) {
		c.starts[idx].changeID = id
		c.starts[idx].change = data
	}
	return func(error) {}, reportChange, func() {}
}

func kvLookup(kv []any, key string) (any, bool) {
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok && k == key {
			return kv[i+1], true
		}
	}
	return nil, false
}

func TestUnit_InstanceEventSink_ReportsEventThroughTracker(t *testing.T) {
	tracker := &captureTracker{}
	sink := newInstanceEventSink(tracker)

	ev := agentinstance.Event{
		Kind:       agentinstance.EventUnsupervisedDeny,
		InstanceID: "inst-1",
		AgentID:    "agent-1",
		AgentName:  "ext-agent",
		SessionID:  libacp.SessionID("sess-1"),
		Time:       time.Now().UTC(),
	}
	sink(ev)

	require.Len(t, tracker.starts, 1, "one Event reports exactly one span")
	got := tracker.starts[0]
	require.Equal(t, "unsupervised_permission", got.op, "op is the event kind")
	require.Equal(t, "agent_instance", got.subject)

	instanceID, ok := kvLookup(got.kv, "instance_id")
	require.True(t, ok)
	require.Equal(t, "inst-1", instanceID)
	agentID, ok := kvLookup(got.kv, "agent_id")
	require.True(t, ok)
	require.Equal(t, "agent-1", agentID)
	agentName, ok := kvLookup(got.kv, "agent_name")
	require.True(t, ok)
	require.Equal(t, "ext-agent", agentName)

	require.Equal(t, "inst-1", got.changeID, "the change is keyed by the instance id")
	require.Equal(t, ev, got.change, "the whole (content-free) Event is recorded as the change")
}

func TestUnit_InstanceEventSink_OpMapsEachKind(t *testing.T) {
	tracker := &captureTracker{}
	sink := newInstanceEventSink(tracker)

	for _, kind := range []agentinstance.EventKind{
		agentinstance.EventStateChange,
		agentinstance.EventAttach,
		agentinstance.EventDetach,
		agentinstance.EventUnsupervisedDeny,
	} {
		sink(agentinstance.Event{Kind: kind, InstanceID: "i"})
	}

	require.Len(t, tracker.starts, 4)
	require.Equal(t, "state_change", tracker.starts[0].op)
	require.Equal(t, "attach", tracker.starts[1].op)
	require.Equal(t, "detach", tracker.starts[2].op)
	require.Equal(t, "unsupervised_permission", tracker.starts[3].op)
}

// The Noop tracker path (tracing disabled) must be a safe no-op: the sink still
// records-and-returns without panicking, matching the tracing gate.
func TestUnit_InstanceEventSink_NoopTrackerIsSafe(t *testing.T) {
	sink := newInstanceEventSink(libtracker.NoopTracker{})
	require.NotPanics(t, func() {
		sink(agentinstance.Event{Kind: agentinstance.EventStateChange, InstanceID: "i", State: "running"})
	})
}
