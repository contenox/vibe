// Package mcpworker keeps MCP server connections alive across chain steps. Chains reach databases, Git hosts, and internal tools through here without reconnecting on every step.
package mcpworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/localhooks"
	"github.com/contenox/contenox/runtime/localhooks/mcpoauth"
	"github.com/contenox/contenox/runtime/runtimetypes"
)

// SubjectExecute returns the NATS subject for tool execution on a named MCP server.
func SubjectExecute(name string) string { return "mcp." + name + ".execute" }

// SubjectListTools returns the NATS subject for listing tools on a named MCP server.
func SubjectListTools(name string) string { return "mcp." + name + ".list-tools" }

const (
	SubjectCreated = "mcp.servers.created"
	SubjectDeleted = "mcp.servers.deleted"
)

// MCPToolRequest is the JSON payload sent to mcp.{name}.execute and list-tools.
type MCPToolRequest struct {
	SessionID string         `json:"session_id,omitempty"` // Contextual isolation key
	Tool      string         `json:"tool"`                 // Empty for list-tools
	Args      map[string]any `json:"args"`
}

// MCPToolReply is the JSON payload returned by a worker.
type MCPToolReply struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// MCPToolListReply is returned by mcp.{name}.list-tools.
type MCPToolListReply struct {
	Tools []runtimetypes.MCPTool `json:"tools"`
	Error string                 `json:"error,omitempty"`
}

// MCPDeletedEvent is published on SubjectDeleted.
type MCPDeletedEvent struct {
	Name string `json:"name"`
}

// poolEntry wraps a session pool with its last-access timestamp for idle eviction.
type poolEntry struct {
	pool       *localhooks.MCPSessionPool
	lastAccess time.Time
}

// worker holds the multiplexed pools and NATS subscriptions for one MCP server.
type worker struct {
	serverName string
	cfg        localhooks.MCPServerConfig
	pools      map[string]*poolEntry // Keyed by Contenox SessionID
	mu         sync.Mutex
	cancelFn   context.CancelFunc // cancels the worker's context → NATS auto-unsubscribes
}

// Manager keeps a persistent MCPSessionPool per registered MCP server and
// exposes each via NATS Serve(). It watches lifecycle events so new workers
// are started and old ones are stopped without restarting the process.
type Manager struct {
	mu        sync.Mutex
	workers   map[string]*worker // keyed by server name
	db        runtimetypes.Store
	messenger libbus.Messenger
	tracker   libtracker.ActivityTracker
	rootCtx   context.Context
}

// New creates a Manager, loads all MCP server configs from the DB, and starts
// a persisted session worker for each. It is safe to call in serverapi.New().
func New(ctx context.Context, db runtimetypes.Store, messenger libbus.Messenger, tracker libtracker.ActivityTracker) (*Manager, error) {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	_, report, end := tracker.Start(ctx, "new", "mcp_manager")
	defer end()

	m := &Manager{
		workers:   make(map[string]*worker),
		db:        db,
		messenger: messenger,
		tracker:   tracker,
		rootCtx:   ctx,
	}

	// Load all configs from DB at startup.
	servers, err := db.ListMCPServers(ctx, nil, 1000)
	if err != nil {
		return nil, fmt.Errorf("mcpworker: load servers from DB: %w", err)
	}
	for _, srv := range servers {
		if err := m.StartWorker(ctx, srv); err != nil {
			report("start_failed", map[string]any{"name": srv.Name, "error": err.Error()})
			// non-fatal: continue with other servers
		}
	}
	return m, nil
}

// getOrCreatePool lazily initializes a session pool for a specific Contenox chat session.
// Uses double-checked locking to prevent TOCTOU ghost-connection races.
func (m *Manager) getOrCreatePool(ctx context.Context, w *worker, chatSessionID string) *localhooks.MCPSessionPool {
	if chatSessionID == "" {
		chatSessionID = "default"
	}

	// Fast path: pool already exists.
	w.mu.Lock()
	if entry, ok := w.pools[chatSessionID]; ok {
		entry.lastAccess = time.Now().UTC()
		w.mu.Unlock()
		return entry.pool
	}
	w.mu.Unlock()

	// Slow path: restore prior MCP Session ID from SQLite KV.
	kvKey := fmt.Sprintf("mcp_session:%s:%s", w.serverName, chatSessionID)
	var priorMcpID string
	var raw json.RawMessage
	if err := m.db.GetKV(ctx, kvKey, &raw); err == nil {
		_ = json.Unmarshal(raw, &priorMcpID)
	}

	// Build config for this specific session.
	poolCfg := w.cfg
	poolCfg.MCPSessionID = priorMcpID
	poolCfg.Tracker = m.tracker
	poolCfg.OnSessionID = func(newID string) {
		// Fire-and-forget: persist in background so we never block the HTTP transport.
		go func(id string) {
			pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if id == "" {
				_ = m.db.DeleteKV(pctx, kvKey)
			} else {
				b, _ := json.Marshal(id)
				_ = m.db.SetKV(pctx, kvKey, b)
			}
		}(newID)
	}

	// Create the pool (connection is lazy — happens inside CallTool/ListTools).
	pool := localhooks.NewMCPSessionPool(poolCfg)

	// Double-check: another goroutine may have inserted for this session while we
	// were doing the DB lookup. Return the existing one to avoid ghost connections.
	w.mu.Lock()
	defer w.mu.Unlock()
	if existing, ok := w.pools[chatSessionID]; ok {
		return existing.pool
	}
	w.pools[chatSessionID] = &poolEntry{pool: pool, lastAccess: time.Now().UTC()}
	return pool
}

// StartWorker starts a multiplexing worker for a named server.
// Idempotent: if a worker already exists for that name it is stopped first.
func (m *Manager) StartWorker(ctx context.Context, srv *runtimetypes.MCPServer) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Teardown old worker and cleanly close all multiplexed connections
	if existing, ok := m.workers[srv.Name]; ok {
		existing.cancelFn()
		existing.mu.Lock()
		for _, e := range existing.pools {
			_ = e.pool.Close()
		}
		existing.mu.Unlock()
		delete(m.workers, srv.Name)
	}

	workerCtx, workerCancel := context.WithCancel(ctx)
	w := &worker{
		serverName: srv.Name,
	cfg:        mcpServerToConfig(srv, m.db),
		pools:      make(map[string]*poolEntry),
		cancelFn:   workerCancel,
	}

	// Background idle-pool eviction: reap sessions untouched for 30 minutes.
	// Because Mcp-Session-Id is persisted in SQLite KV, eviction is safe —
	// the next access transparently restores the session from the KV store.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				w.mu.Lock()
				now := time.Now()
				for sid, entry := range w.pools {
					if now.Sub(entry.lastAccess) > 30*time.Minute {
						_ = entry.pool.Close()
						delete(w.pools, sid)
						_, report, end := m.tracker.Start(workerCtx, "evict", "mcp_pool", "server", srv.Name, "session", sid)
						report("pool_evicted", map[string]any{"server": srv.Name, "session": sid})
						end()
					}
				}
				w.mu.Unlock()
			}
		}
	}()

	// EXECUTE HANDLER
	if _, err := m.messenger.Serve(workerCtx, SubjectExecute(srv.Name), func(ctx context.Context, data []byte) ([]byte, error) {
		var req MCPToolRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return errorReply(fmt.Errorf("mcpworker: decode request: %w", err))
		}

		pool := m.getOrCreatePool(ctx, w, req.SessionID)
		result, err := pool.CallTool(ctx, req.Tool, req.Args)
		if err != nil {
			return errorReply(err)
		}
		return okReply(result)
	}); err != nil {
		workerCancel()
		return fmt.Errorf("mcpworker: serve execute %q: %w", srv.Name, err)
	}

	// LIST-TOOLS HANDLER
	if _, err := m.messenger.Serve(workerCtx, SubjectListTools(srv.Name), func(ctx context.Context, data []byte) ([]byte, error) {
		var req MCPToolRequest // Reuse struct just to grab the SessionID
		// Guard: payload may be nil or empty if sent by old callers.
		if len(data) > 0 {
			_ = json.Unmarshal(data, &req)
		}

		pool := m.getOrCreatePool(ctx, w, req.SessionID)
		tools, err := pool.ListTools(ctx)
		if err != nil {
			return errorListReply(err)
		}
		var out []runtimetypes.MCPTool
		for _, t := range tools {
			raw, _ := json.Marshal(t.InputSchema)
			out = append(out, runtimetypes.MCPTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: raw,
			})
		}
		return json.Marshal(MCPToolListReply{Tools: out})
	}); err != nil {
		workerCancel()
		return fmt.Errorf("mcpworker: serve list-tools %q: %w", srv.Name, err)
	}

	m.workers[srv.Name] = w
	_, reportWorkerStart, endWorkerStart := m.tracker.Start(ctx, "start", "mcp_worker", "name", srv.Name, "transport", string(srv.Transport))
	reportWorkerStart("worker_started", map[string]any{"name": srv.Name, "transport": string(srv.Transport)})
	endWorkerStart()
	return nil
}

// StopWorker stops the named worker and closes all its multiplexed sessions.
func (m *Manager) StopWorker(ctx context.Context, name string) {
	_, report, end := m.tracker.Start(ctx, "stop", "mcp_worker", "name", name)
	defer end()
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[name]; ok {
		w.cancelFn()
		w.mu.Lock()
		for _, e := range w.pools {
			_ = e.pool.Close()
		}
		w.mu.Unlock()
		delete(m.workers, name)
		report("worker_stopped", map[string]any{"name": name})
	}
}

// StopAll stops all active workers and releases all MCP sessions and subprocesses.
// Must be called when the process is about to exit to ensure stdio child processes
// (e.g. npx-spawned MCP servers) are terminated.
func (m *Manager) StopAll() {
	ctx := m.rootCtx
	_, report, end := m.tracker.Start(ctx, "stop_all", "mcp_manager")
	defer end()
	m.mu.Lock()
	names := make([]string, 0, len(m.workers))
	for name := range m.workers {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		m.StopWorker(ctx, name)
	}
	report("all_workers_stopped", map[string]any{"count": len(names)})
} // ActiveWorkers returns names of all currently running workers (test helper).
func (m *Manager) ActiveWorkers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.workers))
	for n := range m.workers {
		names = append(names, n)
	}
	return names
}

// WatchEvents subscribes to mcp.servers.created and mcp.servers.deleted.
// When a new server is created via the API on any node, every node picks up
// the event and starts its own worker (NATS queue groups balance calls across them).
func (m *Manager) WatchEvents(ctx context.Context) error {
	_, _, end := m.tracker.Start(ctx, "watch_events", "mcp_manager")
	defer end()
	createdCh := make(chan []byte, 8)
	deletedCh := make(chan []byte, 8)

	if _, err := m.messenger.Stream(ctx, SubjectCreated, createdCh); err != nil {
		return fmt.Errorf("mcpworker: watch created: %w", err)
	}
	if _, err := m.messenger.Stream(ctx, SubjectDeleted, deletedCh); err != nil {
		return fmt.Errorf("mcpworker: watch deleted: %w", err)
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case data, ok := <-createdCh:
				if !ok {
					return
				}
				var srv runtimetypes.MCPServer
				if err := json.Unmarshal(data, &srv); err != nil {
					_, report, end := m.tracker.Start(ctx, "watch", "mcp_event", "event", "created")
					report("decode_error", map[string]any{"error": err.Error()})
					end()
					continue
				}
				if err := m.StartWorker(ctx, &srv); err != nil {
					_, report, end := m.tracker.Start(ctx, "watch", "mcp_event", "event", "created", "name", srv.Name)
					report("start_worker_failed", map[string]any{"name": srv.Name, "error": err.Error()})
					end()
				}
			case data, ok := <-deletedCh:
				if !ok {
					return
				}
				var ev MCPDeletedEvent
				if err := json.Unmarshal(data, &ev); err != nil {
					_, report, end := m.tracker.Start(ctx, "watch", "mcp_event", "event", "deleted")
					report("decode_error", map[string]any{"error": err.Error()})
					end()
					continue
				}
				m.StopWorker(ctx, ev.Name)
			}
		}
	}()
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func mcpServerToConfig(srv *runtimetypes.MCPServer, store runtimetypes.Store) localhooks.MCPServerConfig {
	cfg := localhooks.MCPServerConfig{
		Name:           srv.Name,
		Transport:      localhooks.MCPTransport(srv.Transport),
		Command:        srv.Command,
		Args:           srv.Args,
		URL:            srv.URL,
		ConnectTimeout: time.Duration(srv.ConnectTimeoutSeconds) * time.Second,
	}
	if srv.AuthType != "" {
		cfg.Auth = &localhooks.MCPAuthConfig{
			Type:          localhooks.MCPAuthType(srv.AuthType),
			Token:         srv.AuthToken,
			APIKeyFromEnv: srv.AuthEnvKey,
		}
	}
	if localhooks.MCPAuthType(srv.AuthType) == localhooks.MCPAuthOAuth && store != nil {
		cfg.OAuth = &localhooks.MCPOAuthConfig{
			TokenStore: mcpoauth.NewKVTokenStore(store),
		}
	}
	return cfg
}

func okReply(result any) ([]byte, error) {
	return json.Marshal(MCPToolReply{Result: result})
}

func errorReply(err error) ([]byte, error) {
	return json.Marshal(MCPToolReply{Error: err.Error()})
}

func errorListReply(err error) ([]byte, error) {
	return json.Marshal(MCPToolListReply{Error: err.Error()})
}

// DecodeToolReply decodes a NATS reply from mcp.{name}.execute.
// Used by PersistentRepo — exported so the hooks package can use it.
func DecodeToolReply(data []byte) (any, error) {
	var reply MCPToolReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, fmt.Errorf("mcpworker: decode tool reply: %w", err)
	}
	if reply.Error != "" {
		return nil, errors.New(reply.Error)
	}
	return reply.Result, nil
}

// DecodeListToolsReply decodes a NATS reply from mcp.{name}.list-tools.
func DecodeListToolsReply(data []byte) ([]runtimetypes.MCPTool, error) {
	var reply MCPToolListReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, fmt.Errorf("mcpworker: decode list-tools reply: %w", err)
	}
	if reply.Error != "" {
		return nil, errors.New(reply.Error)
	}
	return reply.Tools, nil
}
