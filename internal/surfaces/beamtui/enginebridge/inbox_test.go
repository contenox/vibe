package enginebridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/operatorinbox"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/internal/surfaces/acpsvc"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// This file drives the operator-inbox subscription over the REAL SQLite bus the
// harness in bridge_test.go builds — the same backend a beam process wires, at
// the same 5ms poll. Nothing is faked: the payloads published here are the JSON
// operatorinbox.Item marshals to, and the assertion is that a Bridge with a bus
// turns them into typed events on the one ordered outlet a surface consumes.

// publishInboxItem writes one operator-inbox item onto the bus in the shape the
// service publishes: the whole Item as JSON. Field names are spelled out rather
// than taken from the service's struct on purpose — the wire is the contract,
// and a test that marshalled the producer's own type would pass even if the two
// sides drifted together into something no released beam can read.
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

// TestUnit_Bridge_InboxSubscriptionSurfacesItems is the whole seam end to end:
// a report that reached no live session is published on the bus, and the Bridge
// hands it to a surface as InboxItemAdded — with no session, because there was
// none, which is the entire reason the inbox exists.
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

	// The event carries NO session, and that is load-bearing rather than
	// incidental: a surface that routed it by session would drop the one class
	// of notice that has nobody watching for it.
	require.Equal(t, libacp.SessionID(""), item.SessionOf())
}

// TestUnit_Bridge_InboxSubscriptionKeepsPublishOrder pins that several arrivals
// reach the surface in the order they were published — the same no-reordering
// promise the notification stream carries, now for the one event that does not
// come off it.
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

// TestUnit_Bridge_InboxSubscriptionDropsUnusablePayloads: unlike a session
// update — which has UnknownUpdate to fall through to — a bus message a
// consumer cannot use has nowhere to go. An InboxItemAdded with no id and no
// summary would ring the bell and put an empty row on the status bar, which is
// a worse answer than silence, so the bridge drops it and keeps serving.
//
// The good item published last is what proves the drop is a SKIP and not a
// wedge: the subscription must still be alive behind the bad payloads.
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

// TestUnit_InboxSubjectMatchesTheProducer pins the duplicated literal against
// the service's own constant. The production file keeps the literal on purpose
// — the wire is the contract, and this package decodes wire shapes rather than
// binding to service types — so this test is the thing that makes the
// duplication safe rather than merely convenient.
func TestUnit_InboxSubjectMatchesTheProducer(t *testing.T) {
	require.Equal(t, operatorinbox.AddedSubject, inboxAddedSubject,
		"the bridge subscribes to a subject the operator inbox does not publish on")
}

// TestUnit_DecodeInboxItem_MatchesTheProducersType is the cross-lane pin: it
// marshals the SERVICE'S OWN operatorinbox.Item — the exact bytes Add stores and
// publishes — and requires this package's mirror to read every field it claims.
//
// The mirror exists so a field added service-side cannot break decoding here
// (see inboxItem), but that tolerance cuts both ways: a field RENAMED there
// would leave this package silently decoding zeroes, and the hand-written
// payloads in the tests above would keep passing, because they were written to
// match the mirror rather than the producer. This is the one test that fails in
// that case, and it is why it imports the service package the others avoid.
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

	// The reason vocabulary is closed and both members must survive the trip:
	// they are what tells an operator whether a supervisor was ever intended.
	item.Reason = operatorinbox.ReasonOperatorFired
	raw, err = json.Marshal(item)
	require.NoError(t, err)
	got, ok = decodeInboxItem(raw)
	require.True(t, ok)
	require.Equal(t, string(operatorinbox.ReasonOperatorFired), got.Reason)
}

// TestUnit_Bridge_NoBusMeansNoInboxEvents pins the documented nil-Bus contract:
// a process that wired no bus gets a fully working Bridge that simply never
// mentions the inbox. It must not fail construction, and it must not panic on
// teardown reaching for a subscription it never took.
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

	// Close is the assertion: it joins every goroutine this package started,
	// so a watcher accidentally started without a bus would time out here.
	require.NoError(t, b.Close())
	require.NoError(t, db.Close())
}
