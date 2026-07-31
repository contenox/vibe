package enginebridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	libacp "github.com/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// This file drives the operator-inbox subscription over the real SQLite bus
// the harness in bridge_test.go builds. Payloads are the JSON
// operatorinbox.Item marshals to, so the assertion is that a Bridge with a
// bus turns them into typed events on the one ordered outlet a surface consumes.

// publishInboxItem writes one operator-inbox item onto the bus in the shape
// the service publishes. Field names are spelled out rather than taken from
// the service's struct, so a drifted wire shape fails here.
func (h *harness) publishInboxItem(ctx context.Context, t *testing.T, item map[string]any) {
	t.Helper()
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	require.NoError(t, h.bus.Publish(ctx, inboxAddedSubject, raw))
}

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

// A report with no live session, published on the bus, surfaces as InboxItemAdded with no session.
func TestUnit_Bridge_InboxSubscriptionSurfacesItems(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h.publishInboxItem(ctx, t, inboxItemJSON("inbox-1", "operator_fired", "result", "4 packages carry a copyleft licence."))

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

	// Empty by design: routing this by session would drop the one notice
	// class that has nobody watching for it.
	require.Equal(t, libacp.SessionID(""), item.SessionOf())
}

// Several inbox arrivals reach the surface in the order they were published.
func TestUnit_Bridge_InboxSubscriptionKeepsPublishOrder(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, id := range []string{"inbox-1", "inbox-2", "inbox-3"} {
		h.publishInboxItem(ctx, t, inboxItemJSON(id, "parent_gone", "blocker", "which retry loop?"))
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

// Unparseable or id-less payloads are dropped without wedging a later, valid item.
func TestUnit_Bridge_InboxSubscriptionDropsUnusablePayloads(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	require.NoError(t, h.bus.Publish(ctx, inboxAddedSubject, []byte(`not json at all`)))
	require.NoError(t, h.bus.Publish(ctx, inboxAddedSubject, []byte(`{"reason":"operator_fired"}`)))
	h.publishInboxItem(ctx, t, inboxItemJSON("inbox-good", "operator_fired", "finding", "three call sites"))

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

// inboxAddedSubject's duplicated literal matches operatorinbox.AddedSubject exactly.
func TestUnit_InboxSubjectMatchesTheProducer(t *testing.T) {
	require.Equal(t, operatorinbox.AddedSubject, inboxAddedSubject,
		"the bridge subscribes to a subject the operator inbox does not publish on")
}

// decodeInboxItem correctly decodes the producer's own operatorinbox.Item marshaled bytes.
func TestUnit_DecodeInboxItem_MatchesTheProducersType(t *testing.T) {
	item := operatorinbox.Item{
		ID:              "inbox-42",
		MissionID:       "mission-23",
		AgentName:       "auditor",
		Intent:          "audit the dependency licences",
		ParentSessionID: "acp-gone",
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

	// Both reason values must survive the round trip.
	item.Reason = operatorinbox.ReasonOperatorFired
	raw, err = json.Marshal(item)
	require.NoError(t, err)
	got, ok = decodeInboxItem(raw)
	require.True(t, ok)
	require.Equal(t, string(operatorinbox.ReasonOperatorFired), got.Reason)
}

// A Bridge built with a nil Bus works fully and never mentions the inbox.
func TestUnit_Bridge_NoBusMeansNoInboxEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(dir, "bridge.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)

	chainPath := filepath.Join(dir, "chain.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(`{"id":"beam-bridge-test","tasks":[{"id":"noop"}]}`), 0o600))
	t.Setenv(testChainEnv, chainPath)
	chains, err := acpsvc.LoadChainRegistryFrom("unused.json", testChainEnv)
	require.NoError(t, err)

	b, err := New(ctx, Deps{
		Engine:        &enginesvc.Engine{},
		DB:            db,
		ChainRegistry: chains,
		WorkspaceID:   "beam-bridge-ws",
		SessionRouter: acpsvc.NewSessionRouter(),
	})
	require.NoError(t, err)
	require.Nil(t, b.inboxSub, "no bus, no subscription")

	// Close joins every goroutine started; an accidental watcher would hang here.
	require.NoError(t, b.Close())
	require.NoError(t, db.Close())
}
