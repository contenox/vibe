package modeldconn

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/liblease"
)

// A live modeld lease is the source of truth for the autodetected engine. This
// drives the real detector (not just the pure serveableFrom): faking a lease in a
// temp data root makes Backend()/Available()/ServeableBackend() report that
// engine, and releasing it returns to empty — with the grace window still
// reporting the last-seen engine for a restart gap.
func TestUnit_ServeableBackend_FromLiveLease(t *testing.T) {
	dir := t.TempDir()
	SetDataRoot(dir)
	t.Cleanup(func() { SetDataRoot("") })
	resetServeableCache()

	if got := Backend(); got != "" {
		t.Fatalf("no lease: Backend()=%q, want empty", got)
	}

	lease, err := liblease.Acquire(
		filepath.Join(dir, "modeld.lease"), 30*time.Second,
		liblease.WithMeta(map[string]string{"backend": "openvino", "endpoint": "127.0.0.1:5000"}),
	)
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}

	if got := Backend(); got != "openvino" {
		t.Fatalf("live lease: Backend()=%q, want openvino", got)
	}
	if !Available() {
		t.Fatal("Available()=false with a fresh lease")
	}
	if got := ServeableBackend(); got != "openvino" {
		t.Fatalf("ServeableBackend()=%q, want openvino", got)
	}

	if err := lease.Release(); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	if got := Backend(); got != "" {
		t.Fatalf("after release: Backend()=%q, want empty", got)
	}
	// Within the grace window the last-observed engine is still reported, so
	// capability advertisement does not flap during a daemon restart.
	if got := ServeableBackend(); got != "openvino" {
		t.Fatalf("within grace after release: ServeableBackend()=%q, want openvino", got)
	}
}
