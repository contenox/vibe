//go:build !windows

package shellsession

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func sttySize(t *testing.T, m *manager, sessionID string) (rows, cols int) {
	t.Helper()
	ctx := ctxWithSession(sessionID)
	pre := m.Read(sessionID, 0, 0).NextOffset
	if _, err := m.Run(ctx, sessionID, "stty size"); err != nil {
		t.Fatalf("Run(stty size): %v", err)
	}
	re := regexp.MustCompile(`(?m)^(\d+) (\d+)\s*$`)
	var match []string
	ok := waitFor(t, 5*time.Second, func() bool {
		out := strings.ReplaceAll(m.Read(sessionID, pre, 0).Content, "\r\n", "\n")
		match = re.FindStringSubmatch(out)
		return match != nil
	})
	if !ok {
		t.Fatalf("stty size never reported a geometry: %q", m.Read(sessionID, pre, 0).Content)
	}
	return atoi(t, match[1]), atoi(t, match[2])
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// TestManager_DefaultGeometry pins that a client reporting no size gets the deliberate default, not the kernel's raw 0x0.
func TestManager_DefaultGeometry(t *testing.T) {
	m := newTestManager(t, time.Minute)
	rows, cols := sttySize(t, m, "sess-default-size")
	if rows != defaultRows || cols != defaultCols {
		t.Fatalf("a shell with no reported size must start at %dx%d, got %dx%d",
			defaultRows, defaultCols, rows, cols)
	}
}

// TestManager_ResizeBeforeFirstRunSizesTheNewShell pins that a size reported before any run still sizes the shell's first spawn.
func TestManager_ResizeBeforeFirstRunSizesTheNewShell(t *testing.T) {
	m := newTestManager(t, time.Minute)
	if r := m.Read("sess-presize", 0, 0); r.Exists {
		t.Fatalf("precondition: no shell should exist yet")
	}
	m.Resize("sess-presize", 40, 100)
	if r := m.Read("sess-presize", 0, 0); r.Exists {
		t.Fatalf("Resize must not spawn a shell on its own")
	}
	rows, cols := sttySize(t, m, "sess-presize")
	if rows != 40 || cols != 100 {
		t.Fatalf("the first shell must be born at the reported size 40x100, got %dx%d", rows, cols)
	}
}

// TestManager_ResizeAppliesToALiveShell pins that a resize while the shell is running takes effect.
func TestManager_ResizeAppliesToALiveShell(t *testing.T) {
	m := newTestManager(t, time.Minute)
	if rows, cols := sttySize(t, m, "sess-live-size"); rows != defaultRows || cols != defaultCols {
		t.Fatalf("precondition: expected the default %dx%d, got %dx%d", defaultRows, defaultCols, rows, cols)
	}
	m.Resize("sess-live-size", 50, 90)
	rows, cols := sttySize(t, m, "sess-live-size")
	if rows != 50 || cols != 90 {
		t.Fatalf("a live shell must adopt the new size 50x90, got %dx%d", rows, cols)
	}
}

// TestManager_ResizeSurvivesTheIdleReaper pins that a respawned shell keeps the session's last known geometry, not the default.
func TestManager_ResizeSurvivesTheIdleReaper(t *testing.T) {
	m := newTestManager(t, 150*time.Millisecond)
	m.Resize("sess-reaped-size", 33, 111)
	if rows, cols := sttySize(t, m, "sess-reaped-size"); rows != 33 || cols != 111 {
		t.Fatalf("precondition: expected 33x111, got %dx%d", rows, cols)
	}
	if !waitFor(t, 3*time.Second, func() bool { return !m.Read("sess-reaped-size", 0, 0).Exists }) {
		t.Fatalf("idle shell was never reaped")
	}
	rows, cols := sttySize(t, m, "sess-reaped-size")
	if rows != 33 || cols != 111 {
		t.Fatalf("the respawned shell must keep the session's last known size 33x111, got %dx%d", rows, cols)
	}
}

// TestManager_ResizeIsTotal pins that every degenerate Resize input is a no-op, never an error, panic, or shell creation.
func TestManager_ResizeIsTotal(t *testing.T) {
	m := newTestManager(t, time.Minute)
	m.Resize("never-seen", 24, 80)
	m.Resize("", 24, 80)
	m.Resize("sess-bad", 0, 80)
	m.Resize("sess-bad", 24, 0)
	m.Resize("sess-bad", -1, -1)
	for _, id := range []string{"never-seen", "", "sess-bad"} {
		if m.Read(id, 0, 0).Exists {
			t.Fatalf("Resize(%q) must not create a shell", id)
		}
	}
	rows, cols := sttySize(t, m, "sess-bad")
	if rows != defaultRows || cols != defaultCols {
		t.Fatalf("a rejected size must not be stored; want the default %dx%d, got %dx%d",
			defaultRows, defaultCols, rows, cols)
	}
}

// TestManager_KillForgetsGeometry pins that Kill drops the session's remembered geometry, unlike the idle reaper.
func TestManager_KillForgetsGeometry(t *testing.T) {
	m := newTestManager(t, time.Minute)
	m.Resize("sess-killed-size", 44, 99)
	if rows, cols := sttySize(t, m, "sess-killed-size"); rows != 44 || cols != 99 {
		t.Fatalf("precondition: expected 44x99, got %dx%d", rows, cols)
	}
	m.Kill("sess-killed-size")
	rows, cols := sttySize(t, m, "sess-killed-size")
	if rows != defaultRows || cols != defaultCols {
		t.Fatalf("a killed session's geometry must not outlive it; want %dx%d, got %dx%d",
			defaultRows, defaultCols, rows, cols)
	}
}
