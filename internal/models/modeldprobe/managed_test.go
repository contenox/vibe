package modeldprobe

import (
	"context"
	"errors"
	"testing"
)

// A Contenox-managed install (what `contenox setup` downloads) is located ahead
// of PATH, without CONTENOX_MODELD_BIN set.
func TestLocate_ManagedInstall(t *testing.T) {
	const dataRoot = "/data"
	const managed = "/data/modeld/v1.0.0/linux-amd64/modeld"

	d := &Detector{
		dataRoot: dataRoot,
		findManaged: func(_ context.Context, root string) (string, error) {
			if root != dataRoot {
				t.Fatalf("findManaged root = %q, want %q", root, dataRoot)
			}
			return managed, nil
		},
		statBinary: func(p string) bool { return p == managed },
		lookPath:   func(string) (string, error) { return "/usr/bin/modeld", nil }, // must be ignored
	}
	if got := d.locate(); got != managed {
		t.Fatalf("locate = %q, want managed path %q", got, managed)
	}
}

// When the managed install is absent, discovery falls through to PATH.
func TestLocate_ManagedMissingFallsBackToPATH(t *testing.T) {
	d := &Detector{
		dataRoot:    "/data",
		findManaged: func(context.Context, string) (string, error) { return "", errors.New("none") },
		statBinary:  func(string) bool { return false },
		lookPath:    func(string) (string, error) { return "/usr/bin/modeld", nil },
	}
	if got := d.locate(); got != "/usr/bin/modeld" {
		t.Fatalf("locate = %q, want PATH fallback /usr/bin/modeld", got)
	}
}

// New wires version-independent managed discovery.
func TestNew_WiresManagedDiscovery(t *testing.T) {
	dataRoot := t.TempDir()
	d := New(dataRoot)
	if d.dataRoot != dataRoot {
		t.Fatalf("dataRoot = %q, want %q", d.dataRoot, dataRoot)
	}
	if d.findManaged == nil {
		t.Fatalf("findManaged is nil")
	}
}
