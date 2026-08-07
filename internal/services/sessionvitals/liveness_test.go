package sessionvitals

import (
	"strings"
	"testing"
	"time"
)

func at(base time.Time, d time.Duration) time.Time { return base.Add(d) }

func TestUnit_OpenBumpCloseLifecycle(t *testing.T) {
	base := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	tr := NewTracker(0)

	if tr.Ticking() {
		t.Fatal("Ticking true before any Open")
	}

	tr.Open(KindTurn, "t1", "thinking", base)
	if !tr.Ticking() || tr.OpenCount() != 1 {
		t.Fatalf("after Open: Ticking=%v OpenCount=%d, want true 1", tr.Ticking(), tr.OpenCount())
	}

	snap, ok := tr.Snapshot("t1", at(base, 2*time.Second), 4)
	if !ok {
		t.Fatal("Snapshot(t1) not found")
	}
	if snap.Elapsed != "2s" || snap.Label != "thinking" || snap.Text != "thinking" || snap.Stalled {
		t.Fatalf("snapshot = %+v, want elapsed 2s, label/text thinking, not stalled", snap)
	}

	tr.Bump("t1", at(base, 3*time.Second))
	snap, _ = tr.Snapshot("t1", at(base, 3*time.Second), 4)
	if snap.Stalled {
		t.Fatalf("stalled right after Bump: %+v", snap)
	}

	tr.Close("t1", at(base, 5*time.Second))
	if tr.Ticking() || tr.OpenCount() != 0 {
		t.Fatalf("after Close: Ticking=%v OpenCount=%d, want false 0", tr.Ticking(), tr.OpenCount())
	}
	snap, ok = tr.Snapshot("t1", at(base, 5*time.Second), 4)
	if !ok || snap.Elapsed != "5s" || snap.Stalled {
		t.Fatalf("closed snapshot = %+v, ok=%v, want elapsed 5s not stalled", snap, ok)
	}
}

func TestUnit_UnknownIDsAreNoOps(t *testing.T) {
	base := time.Now()
	tr := NewTracker(0)

	// Bump/Close on an id never opened must not panic and must not create
	// phantom state.
	tr.Bump("ghost", base)
	tr.Close("ghost", base)
	if tr.OpenCount() != 0 {
		t.Fatalf("OpenCount after no-op Bump/Close = %d, want 0", tr.OpenCount())
	}
	if _, ok := tr.Snapshot("ghost", base, 4); ok {
		t.Fatal("Snapshot(ghost) found an activity that was never opened")
	}

	// Bump/events racing teardown: after Close, further Bumps are no-ops.
	tr.Open(KindShell, "s1", "running", base)
	tr.Close("s1", at(base, 1*time.Second))
	tr.Bump("s1", at(base, 10*time.Second)) // must not resurrect or panic
	snap, ok := tr.Snapshot("s1", at(base, 20*time.Second), 4)
	if !ok {
		t.Fatal("Snapshot(s1) missing after close")
	}
	if snap.Elapsed != "1s" {
		t.Fatalf("post-close Bump changed frozen elapsed: %+v, want 1s", snap)
	}
}

func TestUnit_StallCrossesAtExactlyThreshold(t *testing.T) {
	base := time.Now()
	tr := NewTracker(8 * time.Second)
	tr.Open(KindTurn, "t1", "working", base)

	cases := []struct {
		name string
		d    time.Duration
		want bool
	}{
		{"just before threshold", 7999 * time.Millisecond, false},
		{"exactly at threshold", 8 * time.Second, true},
		{"past threshold", 9 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, ok := tr.Snapshot("t1", at(base, tc.d), 4)
			if !ok {
				t.Fatal("Snapshot not found")
			}
			if snap.Stalled != tc.want {
				t.Fatalf("Stalled at %v = %v, want %v", tc.d, snap.Stalled, tc.want)
			}
			if tc.want && !strings.Contains(snap.Text, "still working") {
				t.Fatalf("stalled Text = %q, want the stall phrasing", snap.Text)
			}
			if !tc.want && snap.Text != snap.Label {
				t.Fatalf("non-stalled Text = %q, want it to equal Label %q", snap.Text, snap.Label)
			}
		})
	}
}

func TestUnit_SpinnerIndexDeterministicAcrossTicks(t *testing.T) {
	base := time.Now()
	tr := NewTracker(0)
	tr.Open(KindTurn, "t1", "streaming", base)

	const frames = 10
	const tickInterval = 130 * time.Millisecond

	var indexes []int
	now := base
	for i := 0; i < 5; i++ {
		now = now.Add(tickInterval)
		snap, _ := tr.Snapshot("t1", now, frames)
		indexes = append(indexes, snap.SpinnerIndex)
	}

	distinct := make(map[int]bool)
	for _, idx := range indexes {
		distinct[idx] = true
	}
	if len(distinct) < 3 {
		t.Fatalf("only %d distinct spinner indexes across 5 ticks of 130ms: %v, want at least 3", len(distinct), indexes)
	}

	// Determinism: recomputing the snapshot at the same now values yields
	// exactly the same sequence (a pure function of elapsed time, not of
	// call count).
	now = base
	for i, want := range indexes {
		now = now.Add(tickInterval)
		snap, _ := tr.Snapshot("t1", now, frames)
		if snap.SpinnerIndex != want {
			t.Fatalf("tick %d: SpinnerIndex = %d on replay, want %d (deterministic)", i, snap.SpinnerIndex, want)
		}
	}
}

func TestUnit_FrozenOnClose(t *testing.T) {
	base := time.Now()
	tr := NewTracker(0)
	tr.Open(KindMission, "m1", "mission running", base)

	closeAt := at(base, 12*time.Second)
	tr.Close("m1", closeAt)

	first, _ := tr.Snapshot("m1", at(closeAt, 1*time.Second), 8)
	second, _ := tr.Snapshot("m1", at(closeAt, 100*time.Second), 8)

	if first != second {
		t.Fatalf("closed snapshot changed over time: %+v vs %+v, want identical (frozen)", first, second)
	}
	if first.Elapsed != "12s" {
		t.Fatalf("frozen Elapsed = %q, want 12s", first.Elapsed)
	}
	if first.Stalled {
		t.Fatal("closed activity reported Stalled=true, want always false")
	}
}

func TestUnit_ConcurrentActivitiesIndependent(t *testing.T) {
	base := time.Now()
	tr := NewTracker(8 * time.Second)

	tr.Open(KindTurn, "turn", "turn running", base)
	tr.Open(KindShell, "shell", "shell running", at(base, 2*time.Second))
	tr.Open(KindMission, "mission", "mission running", at(base, 4*time.Second))

	// Only "turn" gets bumped; the others go silent.
	now := at(base, 20*time.Second)
	tr.Bump("turn", at(base, 19*time.Second))

	turnSnap, _ := tr.Snapshot("turn", now, 4)
	shellSnap, _ := tr.Snapshot("shell", now, 4)
	missionSnap, _ := tr.Snapshot("mission", now, 4)

	if turnSnap.Stalled {
		t.Fatalf("turn stalled despite a recent Bump: %+v", turnSnap)
	}
	if !shellSnap.Stalled {
		t.Fatalf("shell not stalled after 18s silence: %+v", shellSnap)
	}
	if !missionSnap.Stalled {
		t.Fatalf("mission not stalled after 16s silence: %+v", missionSnap)
	}
	if turnSnap.Elapsed != "20s" {
		t.Fatalf("turn Elapsed = %q, want 20s", turnSnap.Elapsed)
	}
	if shellSnap.Elapsed != "18s" {
		t.Fatalf("shell Elapsed = %q, want 18s", shellSnap.Elapsed)
	}
	if missionSnap.Elapsed != "16s" {
		t.Fatalf("mission Elapsed = %q, want 16s", missionSnap.Elapsed)
	}

	if tr.OpenCount() != 3 {
		t.Fatalf("OpenCount = %d, want 3", tr.OpenCount())
	}
}

func TestUnit_AggregatePicksOldest(t *testing.T) {
	base := time.Now()
	tr := NewTracker(0)

	tr.Open(KindTurn, "second", "second activity", at(base, 5*time.Second))
	tr.Open(KindShell, "first", "first activity", base)
	tr.Open(KindMission, "third", "third activity", at(base, 10*time.Second))

	now := at(base, 15*time.Second)
	snap, count, ok := tr.Aggregate(now, 4)
	if !ok {
		t.Fatal("Aggregate reported ok=false with 3 open activities")
	}
	if count != 3 {
		t.Fatalf("Aggregate count = %d, want 3", count)
	}
	if snap.Label != "first activity" {
		t.Fatalf("Aggregate picked %q, want the oldest (\"first activity\")", snap.Label)
	}

	tr.Close("first", now)
	snap, count, ok = tr.Aggregate(now, 4)
	if !ok || count != 2 || snap.Label != "second activity" {
		t.Fatalf("after closing oldest: snap=%+v count=%d ok=%v, want second activity/2/true", snap, count, ok)
	}

	tr.Close("second", now)
	tr.Close("third", now)
	if _, _, ok := tr.Aggregate(now, 4); ok {
		t.Fatal("Aggregate with nothing open reported ok=true")
	}
}

func TestUnit_ElapsedFormattingTable(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{1 * time.Second, "1s"},
		{7 * time.Second, "7s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m00s"},
		{67 * time.Second, "1m07s"},
		{125 * time.Second, "2m05s"},
		{600 * time.Second, "10m00s"},
	}
	base := time.Now()
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			tr := NewTracker(0)
			tr.Open(KindTurn, "id", "label", base)
			snap, _ := tr.Snapshot("id", at(base, tc.d), 4)
			if snap.Elapsed != tc.want {
				t.Fatalf("Elapsed for %v = %q, want %q", tc.d, snap.Elapsed, tc.want)
			}
		})
	}
}

func TestUnit_DefaultStallThreshold(t *testing.T) {
	base := time.Now()
	tr := NewTracker(0)
	tr.Open(KindTurn, "t1", "working", base)

	snap, _ := tr.Snapshot("t1", at(base, 7*time.Second), 4)
	if snap.Stalled {
		t.Fatal("stalled before the default 8s threshold")
	}
	snap, _ = tr.Snapshot("t1", at(base, 8*time.Second), 4)
	if !snap.Stalled {
		t.Fatal("not stalled at the default 8s threshold")
	}
}
