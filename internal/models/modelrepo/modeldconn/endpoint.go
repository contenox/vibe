package modeldconn

import (
	"context"
	"fmt"
	"sync"

	"github.com/contenox/contenox/internal/models/modeldprobe"
	transportgrpc "github.com/contenox/contenox/libtransport/grpc"
)

// LocalSentinel is the backend BaseURL value meaning "this modeld backend row
// is the local daemon" — reach it via the lease (LocalEndpointAddr), not a
// stored address. Mirrors the existing "local" compatibility alias already
// accepted for --type.
const LocalSentinel = "local"

// LocalEndpointAddr resolves the local modeld owner's current advertised
// endpoint address via the lease, for callers that want to reach it through
// the same fenced Endpoint path used for remote nodes (observe-only backend
// reconcile) rather than the hot chat-serving path's dedicated single-slot
// cache (OpenSession/Describe/Embed/LoadModel above, untouched by this file).
func LocalEndpointAddr(ctx context.Context) (string, error) {
	st := detector().Probe(ctx)
	if st.State != modeldprobe.StateRunning {
		return "", st.Err()
	}
	return st.Endpoint, nil
}

// EndpointHealth reports what a node's Health RPC returned, cached alongside
// its connection.
type EndpointHealth struct {
	InstanceID string
	Backend    string // "llama" | "openvino" | "none"
	Ready      bool
}

// EndpointClient is a fenced connection to one modeld node (local or remote),
// reached by address rather than lease discovery.
type EndpointClient struct {
	*transportgrpc.Client
	EndpointHealth
}

// endpointEntry is the per-backend cache entry: the fenced client plus the
// instance id it was fenced to, so a node restart (new instance id) is
// detected and triggers a redial instead of silently failing every call with
// ErrStaleFence forever.
type endpointEntry struct {
	client   *transportgrpc.Client
	instance string
}

// endpoints is a per-backend-ID connection cache entirely separate from the
// package-level client/key pair used by OpenSession/Status/LoadModel/
// Describe/Embed above. That pair is a deliberate SINGLE-slot cache for the
// local hot chat-serving path (one daemon, one owner at a time); this map
// holds one entry PER REGISTERED MODELD BACKEND (local or remote) for the
// observe-only reconcile path. The two must never share state: dialing a
// remote node must never evict or close the local daemon's cached
// connection, and vice versa.
var (
	endpointsMu sync.Mutex
	endpoints   = map[string]*endpointEntry{}
)

// Endpoint dials (or reuses a cached connection to) the modeld node
// identified by backendID at addr. It health-probes first — unfenced, since
// the caller has no lease file to read a remote node's owner instance id
// from — to learn the node's current instance id, then dials (or confirms) a
// client fenced to it. A cached connection whose remembered instance id no
// longer matches the node's live Health response (the node restarted) is
// dropped and redialed automatically, mirroring the local dial()'s
// take-over-redials behavior above.
func Endpoint(ctx context.Context, backendID, addr string) (EndpointClient, error) {
	if entry, ok := cachedEndpoint(backendID); ok {
		if health, err := entry.client.Health(ctx); err == nil && health.Ready && health.InstanceID == entry.instance {
			return EndpointClient{Client: entry.client, EndpointHealth: EndpointHealth{
				InstanceID: health.InstanceID, Backend: health.Backend, Ready: health.Ready,
			}}, nil
		}
		dropEndpoint(backendID, entry.client)
	}

	probe, err := transportgrpc.DialLeader(addr, "")
	if err != nil {
		return EndpointClient{}, fmt.Errorf("modeldconn: dial %s: %w", addr, err)
	}
	health, err := probe.Health(ctx)
	if err != nil {
		_ = probe.Close()
		return EndpointClient{}, fmt.Errorf("modeldconn: health probe %s: %w", addr, err)
	}
	if !health.Ready || health.InstanceID == "" {
		_ = probe.Close()
		return EndpointClient{}, fmt.Errorf("modeldconn: node %s reported not ready", addr)
	}

	// The probe connection is already fenced to nothing (owner ""); reuse it
	// as-is going forward rather than dialing a second connection — the wire
	// client stores the expected owner client-side only (in the Fence
	// metadata it attaches per call via withOwner), so a fresh Client bound
	// to health.InstanceID is needed for subsequent calls to be fenced.
	_ = probe.Close()
	fenced, err := transportgrpc.DialLeader(addr, health.InstanceID)
	if err != nil {
		return EndpointClient{}, fmt.Errorf("modeldconn: dial %s: %w", addr, err)
	}

	endpointsMu.Lock()
	endpoints[backendID] = &endpointEntry{client: fenced, instance: health.InstanceID}
	endpointsMu.Unlock()

	return EndpointClient{Client: fenced, EndpointHealth: EndpointHealth{
		InstanceID: health.InstanceID, Backend: health.Backend, Ready: health.Ready,
	}}, nil
}

func cachedEndpoint(backendID string) (*endpointEntry, bool) {
	endpointsMu.Lock()
	defer endpointsMu.Unlock()
	entry, ok := endpoints[backendID]
	return entry, ok
}

// dropEndpoint removes and closes a cached connection if it is still the one
// passed in (guards against a race where two callers both decided to redial).
func dropEndpoint(backendID string, stale *transportgrpc.Client) {
	endpointsMu.Lock()
	entry, ok := endpoints[backendID]
	if ok && entry.client == stale {
		delete(endpoints, backendID)
	}
	endpointsMu.Unlock()
	if ok {
		_ = entry.client.Close()
	}
}

// CloseEndpoint drops and closes a cached endpoint connection, e.g. when its
// backend row is deleted from the registry.
func CloseEndpoint(backendID string) {
	endpointsMu.Lock()
	entry, ok := endpoints[backendID]
	delete(endpoints, backendID)
	endpointsMu.Unlock()
	if ok {
		_ = entry.client.Close()
	}
}
