package agentinstance

import (
	"context"
	"testing"

	"github.com/contenox/contenox/libacp"
)

// TestUnit_ViewerHub_JournalSnapshot pins that every delivered update is
// journaled whether or not a viewer is attached, and an unknown session
// yields nil.
func TestUnit_ViewerHub_JournalSnapshot(t *testing.T) {
	hub := newViewerHub("inst-1", 512)
	ctx := context.Background()
	const sid = libacp.SessionID("s1")

	if snap := hub.journalSnapshot(sid); snap != nil {
		t.Fatalf("unknown session must snapshot to nil, got %d entries", len(snap))
	}

	edit := libacp.SessionNotification{
		SessionID: sid,
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			Kind:          libacp.ToolKindEdit,
			ToolContent:   []libacp.ToolCallContent{{Type: libacp.ToolCallContentDiff, Path: "/ws/a.txt", OldText: "", NewText: "hi"}},
		},
	}
	hub.deliver(ctx, edit)

	snap := hub.journalSnapshot(sid)
	if len(snap) != 1 {
		t.Fatalf("want 1 journaled update, got %d", len(snap))
	}
	got := snap[0].Update.ToolContent
	if len(got) != 1 || got[0].Path != "/ws/a.txt" || got[0].NewText != "hi" {
		t.Fatalf("diff content did not round-trip through the journal: %+v", got)
	}
}

// TestUnit_SessionDriver_Cwd pins that a session's cwd is retained and read
// back verbatim, "" for an unknown or never-set session.
func TestUnit_SessionDriver_Cwd(t *testing.T) {
	sd := newSessionDriver()
	const sid = libacp.SessionID("s1")

	if got := sd.cwd(sid); got != "" {
		t.Fatalf("unknown session cwd must be empty, got %q", got)
	}

	ds := sd.get(sid)
	if got := ds.getCwd(); got != "" {
		t.Fatalf("a session opened without a cwd reads empty, got %q", got)
	}

	ds.setCwd("/ws/project")
	if got := sd.cwd(sid); got != "/ws/project" {
		t.Fatalf("cwd = %q, want /ws/project", got)
	}
}
