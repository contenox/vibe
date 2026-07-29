package modeldprobe

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/liblease"
	"github.com/contenox/contenox/internal/transport"
	transportgrpc "github.com/contenox/contenox/internal/transport/grpc"
)

// serveModeld starts a real gRPC transport server on a loopback port serving as
// owner instanceID, and returns its endpoint. The server stops at test end.
func serveModeld(t *testing.T, instanceID string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = transportgrpc.Serve(ctx, lis, transport.NewMemoryService(), instanceID, "llama") }()
	return lis.Addr().String()
}

// detectorFor builds a Detector pointed at leasePath with the binary "found" and
// the real gRPC health ping wired in.
func detectorFor(leasePath string) *Detector {
	return &Detector{
		leasePath:  leasePath,
		lookPath:   func(string) (string, error) { return "/opt/modeld", nil },
		statBinary: func(string) bool { return true },
		now:        time.Now,
		health:     grpcHealthCheck,
	}
}

// leaseAt writes a fresh lease advertising endpoint, and returns its instance id.
func leaseAt(t *testing.T, leasePath, endpoint string) string {
	t.Helper()
	l, err := liblease.Acquire(leasePath, 30*time.Second, liblease.WithMeta(map[string]string{endpointMetaKey: endpoint, backendMetaKey: "llama"}))
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	t.Cleanup(func() { _ = l.Release() })
	rec, err := liblease.Inspect(leasePath)
	if err != nil {
		t.Fatalf("inspect lease: %v", err)
	}
	return rec.InstanceID
}

func TestProbe_HealthyOwnerIsRunning(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	endpoint := "placeholder"
	// Reserve the port first so the lease can advertise it, then serve the lease's
	// own instance id (a consistent, live owner).
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	endpoint = lis.Addr().String()
	instance := leaseAt(t, leasePath, endpoint)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = transportgrpc.Serve(ctx, lis, transport.NewMemoryService(), instance, "llama") }()

	st := detectorFor(leasePath).Probe(context.Background())
	if st.State != StateRunning {
		t.Fatalf("state = %s, want running; err=%v", st.State, st.Err())
	}
	if st.Backend != "llama" {
		t.Fatalf("backend = %q, want llama", st.Backend)
	}
}

func TestProbe_FreshLeaseButDeadEndpointIsUnreachable(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	// Advertise a port nobody is listening on.
	leaseAt(t, leasePath, "127.0.0.1:1") // port 1: not served
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	st := detectorFor(leasePath).Probe(ctx)
	if st.State != StateUnreachable {
		t.Fatalf("state = %s, want unreachable", st.State)
	}
	if !errors.Is(st.Err(), ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", st.Err())
	}
}

func TestProbe_InstanceMismatchIsUnreachable(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	// A different instance is actually serving than the lease names (e.g. a
	// takeover in progress): the owner the lease points to is not the one
	// answering, so it must not read as healthy.
	endpoint := serveModeld(t, "someone-else")
	leaseAt(t, leasePath, endpoint)

	st := detectorFor(leasePath).Probe(context.Background())
	if st.State != StateUnreachable {
		t.Fatalf("state = %s, want unreachable (instance mismatch)", st.State)
	}
}

// Detect() stays network-free: a fresh lease reads as running without any ping,
// even when no server is listening.
func TestDetect_StaysOfflineProxy(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), leaseFileName)
	leaseAt(t, leasePath, "127.0.0.1:1")
	st := detectorFor(leasePath).Detect()
	if st.State != StateRunning {
		t.Fatalf("Detect state = %s, want running (offline proxy)", st.State)
	}
}
