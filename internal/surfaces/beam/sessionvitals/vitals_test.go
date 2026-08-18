package sessionvitals

import (
	"testing"
	"time"

	libacp "github.com/contenox/contenox/libacp"
)

func TestUnit_ContextUsage_UnknownUntilSizeReported(t *testing.T) {
	var u ContextUsage
	if u.Known() {
		t.Fatal("Known true with a zero Size")
	}
	if u.Percent() != 0 || u.Text() != "" || u.Pressure() != PressureNone {
		t.Fatalf("unreported usage produced a reading: %+v %q %s", u.Percent(), u.Text(), u.Pressure())
	}
	// Used without Size is still an absence of information, never 100%.
	u = ContextUsage{Used: 900}
	if u.Known() || u.Percent() != 0 {
		t.Fatalf("Used without Size read as %d%%", u.Percent())
	}
}

func TestUnit_ContextUsage_PressureStepsAtSeventyFiveAndNinety(t *testing.T) {
	for _, tc := range []struct {
		used, size int
		want       Pressure
		wantPct    int
	}{
		{0, 100, PressureNormal, 0},
		{74, 100, PressureNormal, 74},
		{75, 100, PressureHigh, 75},
		{89, 100, PressureHigh, 89},
		{90, 100, PressureCritical, 90},
		{200, 100, PressureCritical, 200},
	} {
		u := ContextUsage{Used: tc.used, Size: tc.size}
		if got := u.Pressure(); got != tc.want {
			t.Fatalf("Pressure(%d/%d) = %s, want %s", tc.used, tc.size, got, tc.want)
		}
		if got := u.Percent(); got != tc.wantPct {
			t.Fatalf("Percent(%d/%d) = %d, want %d", tc.used, tc.size, got, tc.wantPct)
		}
	}
}

func TestUnit_ContextUsage_TextIsTheOneWording(t *testing.T) {
	if got := (ContextUsage{Used: 1200, Size: 8000}).Text(); got != "1200/8000 (15%)" {
		t.Fatalf("Text() = %q", got)
	}
}

func TestUnit_Alerts_NotifiableVocabulary(t *testing.T) {
	for _, r := range []libacp.StopReason{
		libacp.StopReasonEndTurn,
		libacp.StopReasonMaxTokens,
		libacp.StopReasonMaxTurnRequests,
		libacp.StopReasonRefusal,
	} {
		if !NotifiableStop(r) {
			t.Fatalf("NotifiableStop(%s) false", r)
		}
	}
	// The operator cancelled it; they are already here.
	if NotifiableStop(libacp.StopReasonCancelled) {
		t.Fatal("a cancelled turn is notifiable")
	}
	if !NotifiableReport("blocker") || !NotifiableReport("result") {
		t.Fatal("blocker/result are not notifiable")
	}
	if NotifiableReport("progress") || NotifiableReport("") {
		t.Fatal("a progress ping is notifiable")
	}
}

func TestUnit_Alerts_FocusSuppressesUnlessAlways(t *testing.T) {
	base := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	a := NewAlerter(0)

	if a.Ring(base, true, false) {
		t.Fatal("a focused surface rang for a non-blocking fact")
	}
	// Suppressed rings must not consume the rate window either.
	if !a.Ring(base, true, true) {
		t.Fatal("a blocking fact was suppressed by focus")
	}
}

func TestUnit_Alerts_AtMostOneRingPerWindow(t *testing.T) {
	base := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	a := NewAlerter(2 * time.Second)

	if !a.Ring(base, false, false) {
		t.Fatal("the first ring was swallowed")
	}
	if a.Ring(base.Add(1900*time.Millisecond), false, true) {
		t.Fatal("a second ring landed inside the window")
	}
	if !a.Ring(base.Add(2*time.Second), false, false) {
		t.Fatal("the window did not reopen at its edge")
	}
}

func TestUnit_Label_TitleThenNameThenID(t *testing.T) {
	const full = "beam-20a88ab8-4f2e-4b0d-9c31-6f1a2b3c4d5e"
	if got := Label("rewrite the ingest retry", full, "sess-1"); got != "rewrite the ingest retry" {
		t.Fatalf("a title did not win: %q", got)
	}
	if got := Label("", full, "ignored"); got != "beam-20a88ab8" {
		t.Fatalf("name fallback = %q", got)
	}
	if got := Label("", "", full); got != "beam-20a88ab8" {
		t.Fatalf("id fallback = %q", got)
	}
	for _, name := range []string{"beam-0001", "notes", "the ingest rewrite"} {
		if got := ShortName(name); got != name {
			t.Fatalf("ShortName(%q) = %q, want it untouched", name, got)
		}
	}
}

func TestUnit_Roster_ActiveLeadsAndIDLessRowsDrop(t *testing.T) {
	infos := []libacp.SessionInfo{
		{SessionID: "a", Title: "  first  "},
		{SessionID: ""},
		{SessionID: "b"},
		{SessionID: "c", Title: "third"},
	}
	got := Roster(infos, "b", 10)
	if len(got) != 3 {
		t.Fatalf("Roster kept %d rows, want 3", len(got))
	}
	if got[0].ID != "b" || !got[0].Active {
		t.Fatalf("the active row does not lead: %+v", got[0])
	}
	// A titleless row is labelled by its full id, not a shortened one.
	if got[0].Label != "b" {
		t.Fatalf("titleless label = %q", got[0].Label)
	}
	if got[1].Label != "first" {
		t.Fatalf("title was not trimmed: %q", got[1].Label)
	}
	if got[1].Active || got[2].Active {
		t.Fatal("a non-active row is marked active")
	}
	if n := len(Roster(infos, "b", 2)); n != 2 {
		t.Fatalf("limit ignored: %d rows", n)
	}
	if Roster(infos, "b", 0) != nil {
		t.Fatal("a zero limit yielded rows")
	}
}
