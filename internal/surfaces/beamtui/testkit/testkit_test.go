package testkit

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	"github.com/contenox/beam/internal/surfaces/beamtui/liveness"
	libacp "github.com/contenox/beam/libacp"
)

// allFixtures is every SESSION-SCOPED fixture constructor in fixtures.go, named
// for sub-test reporting. Adding a fixture without adding it here is caught by
// nothing automatically — this list is the deliberate, reviewable inventory.
//
// FixtureInboxArrival is deliberately absent: it takes no session id, because
// an operator-inbox item exists precisely because no session was watching. It
// has its own tests below.
func allFixtures() map[string]func(libacp.SessionID) []enginebridge.Event {
	return map[string]func(libacp.SessionID) []enginebridge.Event{
		"StreamingTurn":               FixtureStreamingTurn,
		"StreamingTurnEmptyMessageID": FixtureStreamingTurnEmptyMessageID,
		"TwoTurnsEmptyMessageID":      FixtureTwoTurnsEmptyMessageID,
		"ReplayedHistory":             FixtureReplayedHistory,
		"ThoughtThenAnswer":           FixtureThoughtThenAnswer,
		"MissionReportMidStream":      FixtureMissionReportMidStream,
		"MissionHeartbeat":            FixtureMissionHeartbeat,
		"MissionLifecycle":            FixtureMissionLifecycle,
		"ApprovalFlow":                FixtureApprovalFlow,
		"ShellRun":                    FixtureShellRun,
		"ContextPressure":             FixtureContextPressure,
	}
}

// TestUnit_InboxFixtureIsSessionless pins the one property FixtureInboxArrival
// exists to carry: an inbox item belongs to NO session. A "helpful" session id
// added later — by symmetry with every other fixture in the corpus — would
// quietly assert a routing edge the whole operator inbox exists because of the
// absence of, and a consumer that filtered by session would then look correct
// while dropping exactly the notices nobody is watching for.
func TestUnit_InboxFixtureIsSessionless(t *testing.T) {
	events := FixtureInboxArrival()
	if len(events) == 0 {
		t.Fatal("FixtureInboxArrival produced zero events")
	}
	for i, e := range events {
		if e.SessionOf() != "" {
			t.Errorf("event %d (%T) has session %q, want none", i, e, e.SessionOf())
		}
		item, ok := e.(enginebridge.InboxItemAdded)
		if !ok {
			t.Errorf("event %d is %T, want enginebridge.InboxItemAdded", i, e)
			continue
		}
		if item.ID == "" || item.Reason == "" || item.Summary == "" {
			t.Errorf("event %d is under-populated: %#v", i, item)
		}
	}
	if !reflect.DeepEqual(events, FixtureInboxArrival()) {
		t.Error("FixtureInboxArrival is not deterministic across calls")
	}
}

func TestUnit_FixturesYieldEventsForTheirSession(t *testing.T) {
	const sid = libacp.SessionID("sess-fixture-1")
	for name, build := range allFixtures() {
		t.Run(name, func(t *testing.T) {
			events := build(sid)
			if len(events) == 0 {
				t.Fatalf("fixture %s produced zero events", name)
			}
			for i, e := range events {
				if e.SessionOf() != sid {
					t.Errorf("event %d (%T) has session %q, want %q", i, e, e.SessionOf(), sid)
				}
			}
		})
	}
}

// TestUnit_FixturesAreDeterministic calls each fixture twice with the same
// session id and requires byte-identical (deep-equal) results — the
// "versioned fixture corpus" promise: no time.Now, no randomness, no
// hidden mutable state shared across calls.
func TestUnit_FixturesAreDeterministic(t *testing.T) {
	const sid = libacp.SessionID("sess-fixture-2")
	for name, build := range allFixtures() {
		t.Run(name, func(t *testing.T) {
			first := build(sid)
			second := build(sid)
			if len(first) != len(second) {
				t.Fatalf("fixture %s: call 1 produced %d events, call 2 produced %d", name, len(first), len(second))
			}
			for i := range first {
				// PermissionRequested carries a Resolve func: reflect.DeepEqual
				// treats any two non-nil funcs as unequal (even the same closure
				// compared to itself), so strip it before comparing. Every other
				// field, including pointer fields like UsageCost, DeepEqual
				// compares by pointed-to value, not address, which is what
				// "byte-identical" means here.
				if !reflect.DeepEqual(stripResolve(first[i]), stripResolve(second[i])) {
					t.Errorf("fixture %s: event %d differs between calls:\n- %#v\n+ %#v", name, i, first[i], second[i])
				}
			}
		})
	}
}

// TestUnit_EmptyMessageIDFixturesCarryNoIDs pins the one property those three
// fixtures exist for. They look like ordinary scripts, and an id "helpfully"
// added back — by a copy-paste from the identified variant, or by somebody
// making the corpus look tidy — would silently turn them into duplicates of
// the fixtures they are the counterpart to, and the defect class they cover
// (every assistant message in a process sharing one key) would go untested
// again.
func TestUnit_EmptyMessageIDFixturesCarryNoIDs(t *testing.T) {
	const sid = libacp.SessionID("sess-fixture-3")
	fixtures := map[string]func(libacp.SessionID) []enginebridge.Event{
		"StreamingTurnEmptyMessageID": FixtureStreamingTurnEmptyMessageID,
		"TwoTurnsEmptyMessageID":      FixtureTwoTurnsEmptyMessageID,
		"ReplayedHistory":             FixtureReplayedHistory,
	}
	for name, build := range fixtures {
		t.Run(name, func(t *testing.T) {
			text := 0
			for i, e := range build(sid) {
				var id string
				switch ev := e.(type) {
				case enginebridge.TextDelta:
					id, text = ev.MessageID, text+1
				case enginebridge.ThoughtDelta:
					id, text = ev.MessageID, text+1
				case enginebridge.UserEcho:
					id = ev.MessageID
				default:
					continue
				}
				if id != "" {
					t.Errorf("event %d (%T) carries MessageID %q, want none", i, e, id)
				}
			}
			if text == 0 {
				t.Error("fixture streams no text at all — nothing for the empty id to group")
			}
		})
	}
}

// stripResolve zeroes PermissionRequested.Resolve so two otherwise-identical
// events format identically regardless of closure identity.
func stripResolve(e enginebridge.Event) enginebridge.Event {
	if pr, ok := e.(enginebridge.PermissionRequested); ok {
		pr.Resolve = nil
		return pr
	}
	return e
}

func TestUnit_FakeBridgePlayPreservesOrder(t *testing.T) {
	const sid = libacp.SessionID("sess-order-1")
	script := FixtureStreamingTurn(sid)

	fb := NewFakeBridge()
	fb.Script(script...)
	fb.Play()

	got := make([]enginebridge.Event, 0, len(script))
	for i := 0; i < len(script); i++ {
		select {
		case e, ok := <-fb.Events():
			if !ok {
				t.Fatalf("Events() closed early at index %d", i)
			}
			got = append(got, e)
		default:
			t.Fatalf("Events() had no event ready at index %d (Play should deliver synchronously)", i)
		}
	}

	if !reflect.DeepEqual(got, script) {
		t.Fatalf("Play delivered events out of order or altered:\ngot:  %#v\nwant: %#v", got, script)
	}
}

func TestUnit_FakeBridgeRecordsCalls(t *testing.T) {
	const sid = libacp.SessionID("sess-calls-1")
	fb := NewFakeBridge()

	if err := fb.SubmitPrompt(sid, "hello"); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	if err := fb.Cancel(sid); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := fb.RunShellLine(sid, "ls -la"); err != nil {
		t.Fatalf("RunShellLine: %v", err)
	}
	fb.SetActiveSession(sid)
	if got := fb.ActiveSession(); got != sid {
		t.Fatalf("ActiveSession() = %q, want %q", got, sid)
	}

	want := []string{
		`SubmitPrompt(sess-calls-1, "hello")`,
		`Cancel(sess-calls-1)`,
		`RunShellLine(sess-calls-1, "ls -la")`,
		`SetActiveSession(sess-calls-1)`,
	}
	got := fb.Calls()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Calls() = %#v, want %#v", got, want)
	}

	fb.Close()
	if _, ok := <-fb.Events(); ok {
		t.Fatal("Events() should be closed after Close")
	}
}

func TestUnit_AssertMicroMotionOnActiveTracker(t *testing.T) {
	tr := liveness.NewTracker(8 * time.Second)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr.Open(liveness.KindTurn, "turn-1", "thinking", base)

	// render is a pure function of tick: it derives "now" from a fixed base
	// plus a fixed per-tick advance and reads a liveness.Tracker snapshot
	// through it — never a real clock, exactly the shape this harness
	// documents as the expected contract for render.
	render := func(tick int) string {
		now := base.Add(time.Duration(tick) * 130 * time.Millisecond)
		snap, ok := tr.Snapshot("turn-1", now, 8)
		if !ok {
			t.Fatalf("snapshot missing for turn-1 at tick %d", tick)
		}
		return fmt.Sprintf("%d:%s:%s", snap.SpinnerIndex, snap.Elapsed, snap.Text)
	}

	AssertMicroMotion(t, render, 10)
}

func TestUnit_AssertStabilizesOnClosedTracker(t *testing.T) {
	tr := liveness.NewTracker(8 * time.Second)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr.Open(liveness.KindTurn, "turn-1", "thinking", base)
	tr.Close("turn-1", base.Add(3*time.Second))

	// Once closed, Tracker freezes the snapshot: render must be stable
	// regardless of which "now" each tick derives.
	render := func(tick int) string {
		now := base.Add(time.Duration(tick) * 130 * time.Millisecond)
		snap, ok := tr.Snapshot("turn-1", now, 8)
		if !ok {
			t.Fatalf("snapshot missing for turn-1 at tick %d", tick)
		}
		return fmt.Sprintf("%d:%s:%s", snap.SpinnerIndex, snap.Elapsed, snap.Text)
	}

	AssertStabilizes(t, render, 10)
}
