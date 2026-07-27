//go:build !windows

package shellsession

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// sttySize asks the shell for the PTY's geometry and waits for the answer. It
// reads back through the scrollback rather than trusting Run's snapshot, which
// is a best-effort window and may return before the command has written.
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

// TestManager_DefaultGeometry pins the fallback for a client that never reports
// a size, so the "no size known" path is a deliberate default rather than
// whatever the kernel hands a fresh PTY (0x0, which makes width-aware tools
// guess 80 or misbehave outright).
func TestManager_DefaultGeometry(t *testing.T) {
	m := newTestManager(t, time.Minute)
	rows, cols := sttySize(t, m, "sess-default-size")
	if rows != defaultRows || cols != defaultCols {
		t.Fatalf("a shell with no reported size must start at %dx%d, got %dx%d",
			defaultRows, defaultCols, rows, cols)
	}
}

// TestManager_ResizeBeforeFirstRunSizesTheNewShell is the half of the resize
// contract that a plain ioctl-on-the-live-pty implementation would miss: the
// client knows its geometry before it ever runs a command, and the FIRST
// command must already format against it. The size is therefore remembered by
// session, not by shell, and applied at spawn.
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

// TestManager_ResizeAppliesToALiveShell covers the ordinary case: the window
// changed while the session's shell is already running.
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

// TestManager_ResizeSurvivesTheIdleReaper: the reaper kills an idle shell but
// keeps the session's subscribers, because the panel is still open. The
// geometry belongs to the panel too, so the respawned shell must not silently
// drop back to the default width.
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

// TestManager_ResizeIsTotal: the caller is a UI reporting its own window and has
// no recovery for "that session is gone", so every degenerate input is a no-op
// rather than an error or a panic. Notably Resize must never create a shell.
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
	// A rejected dimension must not be remembered either: the next shell falls
	// back to the default rather than to a half-set geometry.
	rows, cols := sttySize(t, m, "sess-bad")
	if rows != defaultRows || cols != defaultCols {
		t.Fatalf("a rejected size must not be stored; want the default %dx%d, got %dx%d",
			defaultRows, defaultCols, rows, cols)
	}
}

// TestManager_KillForgetsGeometry: Kill is session close/delete, so the size
// goes with the session. The idle reaper is the case that keeps it (covered
// above); this is the case that must not.
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
