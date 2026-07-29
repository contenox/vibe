package modeldconn

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/liblease"
	"github.com/contenox/contenox/internal/modeld/slot"
	"github.com/contenox/contenox/internal/transport"
	transportgrpc "github.com/contenox/contenox/internal/transport/grpc"
)

// fakeNode is a real gRPC-served modeld-shaped server (slot.Service wrapping
// a MemoryService) on a loopback port — the same shape production modeld
// serves, so Endpoint's Health-probe-then-fence dial sequence exercises the
// real wire path rather than a mock.
type fakeNode struct {
	addr     string
	instance string
	cancel   context.CancelFunc
}

func startFakeNode(t *testing.T, addr, instance, backend, modelsDir string) *fakeNode {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	svc := slot.New(
		transport.NewMemoryService(transport.WithOwnerFence(instance)),
		slot.WithOwner(instance),
		slot.WithBackend(backend),
		slot.WithModelsDir(modelsDir),
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = transportgrpc.Serve(ctx, lis, svc, instance, backend) }()
	t.Cleanup(cancel)
	return &fakeNode{addr: lis.Addr().String(), instance: instance, cancel: cancel}
}

func TestUnit_Endpoint_DialsHealthProbesAndCaches(t *testing.T) {
	node := startFakeNode(t, "127.0.0.1:0", "instance-a", "llama", t.TempDir())
	t.Cleanup(func() { CloseEndpoint("backend-1") })

	ec, err := Endpoint(context.Background(), "backend-1", node.addr)
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ec.InstanceID != "instance-a" || ec.Backend != "llama" || !ec.Ready {
		t.Fatalf("EndpointHealth = %+v, want instance-a/llama/ready", ec.EndpointHealth)
	}

	// The embedded *transportgrpc.Client's NodeAdmin/ModelController methods
	// must be reachable through EndpointClient by promotion.
	if _, err := ec.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels via EndpointClient: %v", err)
	}
	if _, err := ec.Status(context.Background()); err != nil {
		t.Fatalf("Status via EndpointClient: %v", err)
	}

	// A second call for the same backend must succeed via the cached
	// connection (still healthy, same instance) without error.
	ec2, err := Endpoint(context.Background(), "backend-1", node.addr)
	if err != nil {
		t.Fatalf("second Endpoint call: %v", err)
	}
	if ec2.InstanceID != "instance-a" {
		t.Fatalf("cached EndpointHealth = %+v, want instance-a", ec2.EndpointHealth)
	}
}

func TestUnit_Endpoint_DifferentBackendsGetIndependentConnections(t *testing.T) {
	nodeA := startFakeNode(t, "127.0.0.1:0", "instance-a", "llama", t.TempDir())
	nodeB := startFakeNode(t, "127.0.0.1:0", "instance-b", "openvino", t.TempDir())
	t.Cleanup(func() { CloseEndpoint("backend-a"); CloseEndpoint("backend-b") })

	ecA, err := Endpoint(context.Background(), "backend-a", nodeA.addr)
	if err != nil {
		t.Fatalf("Endpoint A: %v", err)
	}
	ecB, err := Endpoint(context.Background(), "backend-b", nodeB.addr)
	if err != nil {
		t.Fatalf("Endpoint B: %v", err)
	}
	if ecA.Backend != "llama" || ecB.Backend != "openvino" {
		t.Fatalf("A=%+v B=%+v, want distinct backends untangled", ecA.EndpointHealth, ecB.EndpointHealth)
	}

	// Dialing B must not have disturbed A's cached connection: A must still work.
	if _, err := ecA.Status(context.Background()); err != nil {
		t.Fatalf("Status on A after dialing B: %v", err)
	}
}

func TestUnit_Endpoint_RedialsAfterNodeRestartWithNewInstance(t *testing.T) {
	nodeA := startFakeNode(t, "127.0.0.1:0", "instance-a", "llama", t.TempDir())
	t.Cleanup(func() { CloseEndpoint("backend-1") })

	first, err := Endpoint(context.Background(), "backend-1", nodeA.addr)
	if err != nil {
		t.Fatalf("Endpoint (first): %v", err)
	}
	if first.InstanceID != "instance-a" {
		t.Fatalf("first InstanceID = %q, want instance-a", first.InstanceID)
	}

	// Simulate a daemon restart on the exact same address: stop node A, start
	// a fresh one bound to the identical addr with a different instance id.
	nodeA.cancel()
	waitForPortFree(t, nodeA.addr)
	nodeB := startFakeNode(t, nodeA.addr, "instance-b", "openvino", t.TempDir())

	second, err := Endpoint(context.Background(), "backend-1", nodeB.addr)
	if err != nil {
		t.Fatalf("Endpoint (after restart): %v", err)
	}
	if second.InstanceID != "instance-b" || second.Backend != "openvino" {
		t.Fatalf("after restart EndpointHealth = %+v, want instance-b/openvino (stale cache not detected)", second.EndpointHealth)
	}
}

func TestUnit_Endpoint_UnreachableAddrReturnsError(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close() // nothing listens here now

	_, err = Endpoint(context.Background(), "backend-dead", addr)
	if err == nil {
		t.Fatal("Endpoint against a dead address = nil error, want failure")
	}
}

func TestUnit_CloseEndpoint_DropsCacheWithoutPanicking(t *testing.T) {
	node := startFakeNode(t, "127.0.0.1:0", "instance-a", "llama", t.TempDir())

	if _, err := Endpoint(context.Background(), "backend-1", node.addr); err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	CloseEndpoint("backend-1")
	CloseEndpoint("backend-1") // idempotent: closing twice must not panic

	// A fresh Endpoint call after close must redial cleanly.
	if _, err := Endpoint(context.Background(), "backend-1", node.addr); err != nil {
		t.Fatalf("Endpoint after CloseEndpoint: %v", err)
	}
	CloseEndpoint("backend-1")
}

// LocalEndpointAddr reads the SAME lease file the local hot-path dial()
// reads (via modeldprobe); a real listener is required so Probe's
// reachability check succeeds.
func TestUnit_LocalEndpointAddr_FromLiveLease(t *testing.T) {
	dir := t.TempDir()
	SetDataRoot(dir)
	t.Cleanup(func() { SetDataRoot("") })

	node := startFakeNode(t, "127.0.0.1:0", "instance-local", "llama", t.TempDir())

	lease, err := liblease.Acquire(
		filepath.Join(dir, "modeld.lease"), 30*time.Second,
		liblease.WithMeta(map[string]string{"backend": "llama", "endpoint": node.addr}),
		func(r *liblease.Record) { r.InstanceID = "instance-local" },
	)
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	addr, err := LocalEndpointAddr(context.Background())
	if err != nil {
		t.Fatalf("LocalEndpointAddr: %v", err)
	}
	if addr != node.addr {
		t.Fatalf("LocalEndpointAddr = %q, want %q", addr, node.addr)
	}

	// The resolved address must be usable through the SAME Endpoint() path a
	// remote node uses — proving the local-via-registry-row case (BaseURL ==
	// LocalSentinel) can reach the identical daemon the hot chat-serving path
	// already talks to, without disturbing that path's own connection cache.
	ec, err := Endpoint(context.Background(), "local-backend-row", addr)
	if err != nil {
		t.Fatalf("Endpoint(local): %v", err)
	}
	if ec.InstanceID != "instance-local" {
		t.Fatalf("Endpoint(local).InstanceID = %q, want instance-local", ec.InstanceID)
	}
	CloseEndpoint("local-backend-row")
}

// recordingEmbedService wraps a MemoryService, overriding only Embed so a
// test can prove which node actually received a call and what it returned,
// distinguishing it from a sibling node serving the same test.
type recordingEmbedService struct {
	*transport.MemoryService
	vector []float32
	calls  int
}

func (s *recordingEmbedService) Embed(_ context.Context, _ transport.EmbedRequest) (transport.EmbedResult, error) {
	s.calls++
	return transport.EmbedResult{Vector: s.vector}, nil
}

func startFakeEmbedNode(t *testing.T, addr, instance string, svc *recordingEmbedService) *fakeNode {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	// slot.Service resolves an empty ModelRef.Path by name in its models dir
	// before ever reaching svc.Embed, so a real (if empty) "m/model.gguf" must
	// exist for resolution to succeed — the test is about routing, not resolution.
	modelsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(modelsDir, "m"), 0o755); err != nil {
		t.Fatalf("mkdir model dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelsDir, "m", "model.gguf"), []byte("fake"), 0o644); err != nil {
		t.Fatalf("write fake model: %v", err)
	}
	wrapped := slot.New(svc, slot.WithOwner(instance), slot.WithBackend("llama"), slot.WithModelsDir(modelsDir))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = transportgrpc.Serve(ctx, lis, wrapped, instance, "llama") }()
	t.Cleanup(cancel)
	return &fakeNode{addr: lis.Addr().String(), instance: instance, cancel: cancel}
}

// TestUnit_EmbedTarget_RoutesToTheSpecifiedNodeNotAnySibling is the regression
// test for the gap where llama/openvino embedClient always called the
// ambient local-lease modeldconn.Embed, never a specific target — meaning an
// embed request against a remote/explicit modeld backend could silently run
// on the wrong (local) daemon instead of the one actually registered under
// that BackendID. Two live nodes are served; EmbedTarget must reach exactly
// the one named by BackendID/Endpoint, never the other.
func TestUnit_EmbedTarget_RoutesToTheSpecifiedNodeNotAnySibling(t *testing.T) {
	svcA := &recordingEmbedService{MemoryService: transport.NewMemoryService(transport.WithOwnerFence("instance-a")), vector: []float32{1, 0, 0}}
	svcB := &recordingEmbedService{MemoryService: transport.NewMemoryService(transport.WithOwnerFence("instance-b")), vector: []float32{0, 1, 0}}
	startFakeEmbedNode(t, "127.0.0.1:0", "instance-a", svcA)
	nodeB := startFakeEmbedNode(t, "127.0.0.1:0", "instance-b", svcB)
	t.Cleanup(func() { CloseEndpoint("backend-a"); CloseEndpoint("backend-b") })

	res, err := EmbedTarget(context.Background(),
		ModeldTarget{BackendID: "backend-b", Endpoint: nodeB.addr},
		ModelRef{Name: "m", Type: "llama"}, transport.Config{}, "hello")
	if err != nil {
		t.Fatalf("EmbedTarget: %v", err)
	}
	if len(res.Vector) != 3 || res.Vector[1] != 1 {
		t.Fatalf("EmbedTarget result = %+v, want node B's vector", res)
	}
	if svcB.calls != 1 {
		t.Fatalf("node B calls = %d, want 1", svcB.calls)
	}
	if svcA.calls != 0 {
		t.Fatalf("node A calls = %d, want 0 (request must not reach the sibling node)", svcA.calls)
	}
}

// TestUnit_EmbedTarget_EmptyEndpointFallsBackToLocalLease proves the
// zero-value ModeldTarget (untargeted case) preserves prior behavior by
// routing through the ambient local lease, exactly like OpenSessionTarget
// and DescribeTarget already do for their own empty-Endpoint case.
func TestUnit_EmbedTarget_EmptyEndpointFallsBackToLocalLease(t *testing.T) {
	dir := t.TempDir()
	SetDataRoot(dir)
	t.Cleanup(func() { SetDataRoot("") })

	svc := &recordingEmbedService{MemoryService: transport.NewMemoryService(transport.WithOwnerFence("instance-local")), vector: []float32{2, 4}}
	node := startFakeEmbedNode(t, "127.0.0.1:0", "instance-local", svc)

	lease, err := liblease.Acquire(
		filepath.Join(dir, "modeld.lease"), 30*time.Second,
		liblease.WithMeta(map[string]string{"backend": "llama", "endpoint": node.addr}),
		func(r *liblease.Record) { r.InstanceID = "instance-local" },
	)
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	res, err := EmbedTarget(context.Background(), ModeldTarget{}, ModelRef{Name: "m", Type: "llama"}, transport.Config{}, "hello")
	if err != nil {
		t.Fatalf("EmbedTarget (empty target): %v", err)
	}
	if len(res.Vector) != 2 || res.Vector[0] != 2 {
		t.Fatalf("EmbedTarget result = %+v, want the local lease node's vector", res)
	}
	if svc.calls != 1 {
		t.Fatalf("local node calls = %d, want 1", svc.calls)
	}
}

func waitForPortFree(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		lis, err := net.Listen("tcp", addr)
		if err == nil {
			lis.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %s never freed: %v", addr, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
