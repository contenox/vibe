package mcpworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/mcpworker"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalStore is a tiny Store stub for tests that only need ListMCPServers.
type minimalStore struct {
	runtimetypes.Store // embed to satisfy interface, panics on everything else
}

func (m *minimalStore) ListMCPServers(_ context.Context, _ *time.Time, _ int) ([]*runtimetypes.MCPServer, error) {
	return nil, nil // no servers at startup
}

func (m *minimalStore) GetKV(_ context.Context, _ string, _ interface{}) error {
	return errors.New("no kv") // not found — triggers default session
}

func (m *minimalStore) SetKV(_ context.Context, _ string, _ json.RawMessage) error {
	return nil // fire-and-forget — no-op in tests
}

func newTestManager(ctx context.Context, t *testing.T, bus *libbus.InMem) *mcpworker.Manager {
	t.Helper()
	mgr, err := mcpworker.New(ctx, &minimalStore{}, bus, libtracker.NoopTracker{})
	require.NoError(t, err)
	return mgr
}

// fakeMCPServer returns a config for an MCP server that will fail to connect
// (no real server), but the NATS handler IS registered, so Request() reaches
// the handler and returns an error reply—not ErrRequestTimeout.
func fakeMCPServer(name string) *runtimetypes.MCPServer {
	return &runtimetypes.MCPServer{
		ID:        "00000000-0000-0000-0000-000000000001",
		Name:      name,
		Transport: "sse",
		URL:       "http://127.0.0.1:19999/sse", // nothing listening here
	}
}

// TestUnit_MCPWorker_StartStop verifies that StartWorker registers the NATS
// subjects and StopWorker unregisters them.
func TestUnit_MCPWorker_StartStop(t *testing.T) {
	ctx := t.Context()
	bus := libbus.NewInMem()
	mgr := newTestManager(ctx, t, bus)

	srv := fakeMCPServer("unit-test-server")

	// Before start: Request should time out (no handler).
	_, err := bus.Request(ctx, mcpworker.SubjectExecute(srv.Name), []byte(`{}`))
	require.ErrorIs(t, err, libbus.ErrRequestTimeout, "expected no handler before StartWorker")

	// Start worker — connect will fail (no real server) but handler is registered.
	err = mgr.StartWorker(ctx, srv)
	require.NoError(t, err)

	assert.Contains(t, mgr.ActiveWorkers(), srv.Name)

	// After start: Request reaches the handler (returns an error reply, not ErrRequestTimeout).
	payload, _ := json.Marshal(mcpworker.MCPToolRequest{Tool: "ping", Args: nil})
	replyData, err := bus.Request(ctx, mcpworker.SubjectExecute(srv.Name), payload)
	require.NoError(t, err, "handler should be reachable even if MCP connect fails")
	// The reply contains an error field because the MCP session couldn't be established.
	_, decodeErr := mcpworker.DecodeToolReply(replyData)
	assert.Error(t, decodeErr, "tool call should fail because no real MCP server is running")

	// list-tools subject also registered.
	replyData, err = bus.Request(ctx, mcpworker.SubjectListTools(srv.Name), nil)
	require.NoError(t, err)
	_, _ = mcpworker.DecodeListToolsReply(replyData) // may error, just check handler is reachable

	// Stop worker: subjects should eventually be gone.
	// InMem.Serve unsubscribes via a goroutine on context cancel, so use Eventually.
	mgr.StopWorker(ctx, srv.Name)
	assert.NotContains(t, mgr.ActiveWorkers(), srv.Name)

	require.Eventually(t, func() bool {
		_, err := bus.Request(ctx, mcpworker.SubjectExecute(srv.Name), []byte(`{}`))
		return errors.Is(err, libbus.ErrRequestTimeout)
	}, 2*time.Second, 10*time.Millisecond, "handler should be gone after StopWorker")
}

// TestUnit_MCPWorker_WatchCreated verifies that publishing mcp.servers.created
// causes the Manager to automatically start a new worker.
func TestUnit_MCPWorker_WatchCreated(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	bus := libbus.NewInMem()
	mgr := newTestManager(ctx, t, bus)
	require.NoError(t, mgr.WatchEvents(ctx))

	srv := fakeMCPServer("watch-created-server")

	// Publish created event.
	data, err := json.Marshal(srv)
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, mcpworker.SubjectCreated, data))

	// Give the goroutine a moment to process the event.
	require.Eventually(t, func() bool {
		return contains(mgr.ActiveWorkers(), srv.Name)
	}, 2*time.Second, 50*time.Millisecond, "worker should start after created event")
}

// TestUnit_MCPWorker_WatchDeleted verifies that publishing mcp.servers.deleted
// stops the named worker.
func TestUnit_MCPWorker_WatchDeleted(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	bus := libbus.NewInMem()
	mgr := newTestManager(ctx, t, bus)
	require.NoError(t, mgr.WatchEvents(ctx))

	srv := fakeMCPServer("watch-deleted-server")

	// Start a worker manually.
	require.NoError(t, mgr.StartWorker(ctx, srv))
	require.Contains(t, mgr.ActiveWorkers(), srv.Name)

	// Publish deleted event.
	data, err := json.Marshal(mcpworker.MCPDeletedEvent{Name: srv.Name})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, mcpworker.SubjectDeleted, data))

	// Worker should stop.
	require.Eventually(t, func() bool {
		return !contains(mgr.ActiveWorkers(), srv.Name)
	}, 2*time.Second, 50*time.Millisecond, "worker should stop after deleted event")

	// NATS subject should be gone.
	_, err = bus.Request(ctx, mcpworker.SubjectExecute(srv.Name), []byte(`{}`))
	require.ErrorIs(t, err, libbus.ErrRequestTimeout)
}

// TestUnit_MCPWorker_IdempotentStart verifies that starting a worker that
// already exists replaces it cleanly (no duplicate handlers, no panic).
func TestUnit_MCPWorker_IdempotentStart(t *testing.T) {
	ctx := t.Context()
	bus := libbus.NewInMem()
	mgr := newTestManager(ctx, t, bus)

	srv := fakeMCPServer("idempotent-server")

	require.NoError(t, mgr.StartWorker(ctx, srv))
	require.NoError(t, mgr.StartWorker(ctx, srv)) // second start replaces the first

	workers := mgr.ActiveWorkers()
	count := 0
	for _, w := range workers {
		if w == srv.Name {
			count++
		}
	}
	assert.Equal(t, 1, count, "should only have one worker per name")
}

// TestUnit_MCPWorker_StopNonExistentWorker verifies that stopping a worker
// that was never started is a no-op.
func TestUnit_MCPWorker_StopNonExistentWorker(t *testing.T) {
	ctx := t.Context()
	bus := libbus.NewInMem()
	mgr := newTestManager(ctx, t, bus)

	// Should not panic.
	mgr.StopWorker(ctx, "ghost-server")
	assert.NotContains(t, mgr.ActiveWorkers(), "ghost-server")
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// TestUnit_MCPWorker_BootLoadsFromDB verifies that New() calls ListMCPServers
// and starts a worker for each returned config.
func TestUnit_MCPWorker_BootLoadsFromDB(t *testing.T) {
	ctx := t.Context()
	bus := libbus.NewInMem()

	preloaded := []*runtimetypes.MCPServer{
		fakeMCPServer("boot-server-a"),
		fakeMCPServer("boot-server-b"),
	}
	preloaded[1].ID = "00000000-0000-0000-0000-000000000002"

	store := &preloadedStore{servers: preloaded}
	mgr, err := mcpworker.New(ctx, store, bus, libtracker.NoopTracker{})
	require.NoError(t, err)

	workers := mgr.ActiveWorkers()
	assert.Contains(t, workers, "boot-server-a")
	assert.Contains(t, workers, "boot-server-b")
}

// preloadedStore returns a fixed list from ListMCPServers.
type preloadedStore struct {
	runtimetypes.Store
	servers []*runtimetypes.MCPServer
}

func (s *preloadedStore) ListMCPServers(_ context.Context, _ *time.Time, _ int) ([]*runtimetypes.MCPServer, error) {
	return s.servers, nil
}

// TestUnit_MCPWorker_DecodeToolReply_ErrorPropagation verifies that tool error
// responses from workers are correctly decoded back to Go errors.
func TestUnit_MCPWorker_DecodeToolReply_ErrorPropagation(t *testing.T) {
	data, err := json.Marshal(mcpworker.MCPToolReply{Error: "tool execution failed: file not found"})
	require.NoError(t, err)

	_, decodeErr := mcpworker.DecodeToolReply(data)
	require.Error(t, decodeErr)
	assert.Contains(t, decodeErr.Error(), "file not found")
}

// TestUnit_MCPWorker_DecodeToolReply_Success verifies happy-path decoding.
func TestUnit_MCPWorker_DecodeToolReply_Success(t *testing.T) {
	data, err := json.Marshal(mcpworker.MCPToolReply{Result: "hello world"})
	require.NoError(t, err)

	result, decodeErr := mcpworker.DecodeToolReply(data)
	require.NoError(t, decodeErr)
	assert.Equal(t, "hello world", result)
}

// TestUnit_MCPWorker_DecodeListToolsReply_ErrorPropagation verifies list-tools
// error responses are decoded properly.
func TestUnit_MCPWorker_DecodeListToolsReply_ErrorPropagation(t *testing.T) {
	data, err := json.Marshal(mcpworker.MCPToolListReply{Error: "connection refused"})
	require.NoError(t, err)

	_, decodeErr := mcpworker.DecodeListToolsReply(data)
	require.Error(t, decodeErr)
	assert.Contains(t, decodeErr.Error(), "connection refused")
}

// Compile-time check: the embed satisfies the interface only if minimalStore
// implements the one method we override. This is a safety net.
var _ interface {
	ListMCPServers(context.Context, *time.Time, int) ([]*runtimetypes.MCPServer, error)
} = (*minimalStore)(nil)

var _ error = errors.New("") // suppress unused import warning
