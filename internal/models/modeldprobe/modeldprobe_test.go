package modeldprobe

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/liblease"
)

// newDetector builds a Detector with injected binary lookup and clock, pointed
// at leasePath. It avoids env and real PATH so tests are deterministic.
func newDetector(leasePath string, binaryFound bool, now time.Time) *Detector {
	lookPath := func(string) (string, error) { return "", exec.ErrNotFound }
	if binaryFound {
		lookPath = func(string) (string, error) { return "/opt/modeld", nil }
	}
	return &Detector{
		leasePath:  leasePath,
		lookPath:   lookPath,
		statBinary: func(string) bool { return true },
		now:        func() time.Time { return now },
	}
}

func acquireLease(t *testing.T, leasePath string, ttl time.Duration) {
	t.Helper()
	l, err := liblease.Acquire(leasePath, ttl, liblease.WithMeta(map[string]string{endpointMetaKey: "127.0.0.1:5000"}))
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	t.Cleanup(func() { _ = l.Release() })
}

func TestDetect_NotInstalled(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	st := newDetector(leasePath, false, time.Now()).Detect()
	if st.State != StateNotInstalled {
		t.Fatalf("state = %s, want not-installed", st.State)
	}
	if !errors.Is(st.Err(), ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", st.Err())
	}
}

func TestDetect_NotRunning(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	st := newDetector(leasePath, true, time.Now()).Detect()
	if st.State != StateNotRunning {
		t.Fatalf("state = %s, want not-running", st.State)
	}
	if st.Binary != "/opt/modeld" {
		t.Fatalf("binary = %q, want /opt/modeld", st.Binary)
	}
	if !errors.Is(st.Err(), ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", st.Err())
	}
}

func TestDetect_Stale(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	acquireLease(t, leasePath, 30*time.Second)
	// Look at the lease from far enough in the future that it has expired.
	future := time.Now().Add(time.Hour)
	st := newDetector(leasePath, true, future).Detect()
	if st.State != StateStale {
		t.Fatalf("state = %s, want stale", st.State)
	}
	if !errors.Is(st.Err(), ErrStale) {
		t.Fatalf("err = %v, want ErrStale", st.Err())
	}
}

func TestDetect_Running(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	acquireLease(t, leasePath, 30*time.Second)
	st := newDetector(leasePath, true, time.Now()).Detect()
	if st.State != StateRunning {
		t.Fatalf("state = %s, want running", st.State)
	}
	if st.Endpoint != "127.0.0.1:5000" {
		t.Fatalf("endpoint = %q, want 127.0.0.1:5000", st.Endpoint)
	}
	if st.Err() != nil {
		t.Fatalf("err = %v, want nil for running", st.Err())
	}
}

// A live lease means running even if no local binary can be located (the daemon
// was started elsewhere, e.g. from an extension dir).
func TestDetect_RunningPrecedesBinary(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	acquireLease(t, leasePath, 30*time.Second)
	st := newDetector(leasePath, false, time.Now()).Detect()
	if st.State != StateRunning {
		t.Fatalf("state = %s, want running", st.State)
	}
}

// An explicit override that does not exist on disk is treated as not installed.
func TestLocate_OverrideMissing(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	d := &Detector{
		leasePath:      leasePath,
		binaryOverride: "/nope/modeld",
		statBinary:     func(string) bool { return false },
		lookPath:       func(string) (string, error) { return "/opt/modeld", nil }, // must be ignored
		now:            time.Now,
	}
	if got := d.locate(); got != "" {
		t.Fatalf("locate = %q, want empty when override is missing", got)
	}
}

// An explicit override that exists wins over PATH.
func TestLocate_OverrideWins(t *testing.T) {
	d := &Detector{
		binaryOverride: "/ext/modeld",
		statBinary:     func(string) bool { return true },
		lookPath:       func(string) (string, error) { return "/usr/bin/modeld", nil },
	}
	if got := d.locate(); got != "/ext/modeld" {
		t.Fatalf("locate = %q, want /ext/modeld", got)
	}
}
