package enginebridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func inboxItemJSON(id, reason, kind, summary string) map[string]any {
	return map[string]any{
		"id":        id,
		"missionId": "mission-23",
		"agentName": "auditor",
		"intent":    "audit the dependency licences",
		"reason":    reason,
		"report": map[string]any{
			"id":        "rep-" + id,
			"missionId": "mission-23",
			"kind":      kind,
			"summary":   summary,
			"createdAt": "2026-07-27T10:00:00Z",
		},
		"createdAt": "2026-07-27T10:00:01Z",
	}
}

func mustJSON(v map[string]any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestUnit_Bridge_InboxRelaySurfacesItems(t *testing.T) {
	h := newHarness(t)

	h.inbox <- mustJSON(inboxItemJSON("inbox-1", "operator_fired", "result", "4 packages carry a copyleft licence."))

	events := h.collect(10*time.Second, func(ev Event) bool {
		_, ok := ev.(InboxItemAdded)
		return ok
	})
	item, ok := firstOfType[InboxItemAdded](events)
	require.True(t, ok)
	require.Equal(t, "inbox-1", item.ID)
	require.Equal(t, "mission-23", item.MissionID)
	require.Equal(t, "auditor", item.AgentName)
	require.Equal(t, "audit the dependency licences", item.Intent)
	require.Equal(t, "operator_fired", item.Reason)
	require.Equal(t, "result", item.Kind)
	require.Equal(t, "4 packages carry a copyleft licence.", item.Summary)
	require.Equal(t, libacp.SessionID(""), item.SessionOf())
}

func TestUnit_Bridge_InboxRelaySurvivesTheActiveSessionFilter(t *testing.T) {
	h := newHarness(t)
	h.bridge.SetActiveSession("beam-live")

	h.inbox <- mustJSON(inboxItemJSON("inbox-1", "parent_gone", "blocker", "which retry loop?"))

	events := h.collect(10*time.Second, func(ev Event) bool {
		_, ok := ev.(InboxItemAdded)
		return ok
	})
	_, ok := firstOfType[InboxItemAdded](events)
	require.True(t, ok, "a sessionless notice must not be dropped by the active-session filter")
}

func TestUnit_Bridge_InboxRelayKeepsPublishOrder(t *testing.T) {
	h := newHarness(t)

	for _, id := range []string{"inbox-1", "inbox-2", "inbox-3"} {
		h.inbox <- mustJSON(inboxItemJSON(id, "parent_gone", "blocker", "which retry loop?"))
	}

	var got []string
	h.collect(10*time.Second, func(ev Event) bool {
		if item, ok := ev.(InboxItemAdded); ok {
			got = append(got, item.ID)
		}
		return len(got) == 3
	})
	require.Equal(t, []string{"inbox-1", "inbox-2", "inbox-3"}, got)
}

func TestUnit_Bridge_InboxRelayDropsUnusablePayloads(t *testing.T) {
	h := newHarness(t)

	h.inbox <- []byte(`not json at all`)
	h.inbox <- []byte(`{"reason":"operator_fired"}`)
	h.inbox <- mustJSON(inboxItemJSON("inbox-good", "operator_fired", "finding", "three call sites"))

	var seen []InboxItemAdded
	h.collect(10*time.Second, func(ev Event) bool {
		item, ok := ev.(InboxItemAdded)
		if ok {
			seen = append(seen, item)
		}
		return ok
	})
	require.Len(t, seen, 1, "an id-less or unparseable payload must not reach the surface")
	require.Equal(t, "inbox-good", seen[0].ID)
}

func TestUnit_Bridge_NoInboxChannelMeansNoRelay(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Inbox = nil })

	sid := h.initSession(context.Background())
	h.notify(sid, libacp.NewAgentMessageChunk("marker"))

	events := h.collect(10*time.Second, func(ev Event) bool {
		td, ok := ev.(TextDelta)
		return ok && td.Text == "marker"
	})
	_, sawInbox := firstOfType[InboxItemAdded](events)
	require.False(t, sawInbox)

	require.NoError(t, h.bridge.Close(), "Close joins every goroutine; an accidental relay would hang here")
}

func TestUnit_DecodeInboxItem_MatchesTheProducersType(t *testing.T) {
	item := operatorinbox.Item{
		ID:              "inbox-42",
		MissionID:       "mission-23",
		AgentName:       "auditor",
		Intent:          "audit the dependency licences",
		ParentSessionID: "beam-gone",
		Reason:          operatorinbox.ReasonParentGone,
		Report: missionservice.Report{
			ID:        "rep-1",
			MissionID: "mission-23",
			Kind:      missionservice.ReportKindResult,
			Summary:   "4 packages carry a copyleft licence.",
			Detail:    "full list attached",
			CreatedAt: time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC),
		},
		CreatedAt: time.Date(2026, 7, 27, 10, 0, 1, 0, time.UTC),
	}
	raw, err := json.Marshal(item)
	require.NoError(t, err)

	got, ok := decodeInboxItem(raw)
	require.True(t, ok, "the producer's own item must decode")
	require.Equal(t, InboxItemAdded{
		ID:        "inbox-42",
		MissionID: "mission-23",
		AgentName: "auditor",
		Intent:    "audit the dependency licences",
		Reason:    string(operatorinbox.ReasonParentGone),
		Kind:      string(missionservice.ReportKindResult),
		Summary:   "4 packages carry a copyleft licence.",
	}, got)

	item.Reason = operatorinbox.ReasonOperatorFired
	raw, err = json.Marshal(item)
	require.NoError(t, err)
	got, ok = decodeInboxItem(raw)
	require.True(t, ok)
	require.Equal(t, string(operatorinbox.ReasonOperatorFired), got.Reason)
}

func TestUnit_DecodeInboxItem_RejectsUnusablePayloads(t *testing.T) {
	_, ok := decodeInboxItem([]byte(`not json`))
	require.False(t, ok)

	_, ok = decodeInboxItem([]byte(`{"reason":"operator_fired"}`))
	require.False(t, ok, "an item with no id has nothing a consumer could act on")
}
