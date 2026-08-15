package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/mcpworker"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
)

// defaultDeclaredRemoteTimeoutMs matches `contenox tools add`'s own default so
// a declared service behaves like a registered one.
const defaultDeclaredRemoteTimeoutMs = 30000

// reconcileDeclaredTools makes the declaration-scoped MCP servers and remote
// tools on this machine exactly the set the current declarations ask for.
//
// Reconciliation rather than event bookkeeping: the desired set is recomputed
// from the declarations every pass, so a crash between writing a row and
// writing sync state cannot strand a registration, and deleting a declaration
// retires what it brought without anything having recorded that it existed.
//
// Rows carry the runtimetypes.DeclaredToolNamePrefix, which is what separates
// them from `contenox mcp add` / `contenox tools add` registrations — those are
// the operator's and are never touched here.
func reconcileDeclaredTools(ctx context.Context, store runtimetypes.Store, bus libbus.Messenger, results []agentdecl.SyncResult) []agentdecl.SyncResult {
	desiredMCP := map[string]*runtimetypes.MCPServer{}
	desiredRemote := map[string]*runtimetypes.RemoteTools{}
	var problems []agentdecl.SyncResult

	for _, res := range results {
		switch res.Action {
		case agentdecl.ActionRefused, agentdecl.ActionIgnored:
			// A declaration that produced no agent registers nothing.
			continue
		}
		for _, srv := range res.MCP {
			name := runtimetypes.DeclaredToolName(res.Name, srv.Declared)
			desiredMCP[name] = declaredMCPRow(name, srv)
		}
		for _, tool := range res.Remote {
			name := runtimetypes.DeclaredToolName(res.Name, tool.Declared)
			desiredRemote[name] = declaredRemoteRow(name, tool)
		}
	}

	problems = append(problems, reconcileDeclaredMCP(ctx, store, bus, desiredMCP)...)
	problems = append(problems, reconcileDeclaredRemote(ctx, store, desiredRemote)...)
	return problems
}

func reconcileDeclaredMCP(ctx context.Context, store runtimetypes.Store, bus libbus.Messenger, desired map[string]*runtimetypes.MCPServer) []agentdecl.SyncResult {
	var problems []agentdecl.SyncResult

	existing, err := listDeclaredMCPNames(ctx, store)
	if err != nil {
		return append(problems, declaredProblem("mcp", "could not read existing registrations: "+err.Error()))
	}

	for _, name := range sortedRowNames(desired) {
		if err := store.UpsertMCPServerByName(ctx, desired[name]); err != nil {
			problems = append(problems, declaredProblem(name, "could not register: "+err.Error()))
			continue
		}
		publishMCPEvent(ctx, bus, mcpworker.SubjectCreated, desired[name])
	}

	for _, name := range existing {
		if _, kept := desired[name]; kept {
			continue
		}
		row, err := store.GetMCPServerByName(ctx, name)
		if err != nil {
			continue
		}
		if err := store.DeleteMCPServer(ctx, row.ID); err != nil && !errors.Is(err, libdb.ErrNotFound) {
			problems = append(problems, declaredProblem(name, "could not retire: "+err.Error()))
			continue
		}
		publishMCPDeleted(ctx, bus, name)
	}
	return problems
}

func reconcileDeclaredRemote(ctx context.Context, store runtimetypes.Store, desired map[string]*runtimetypes.RemoteTools) []agentdecl.SyncResult {
	var problems []agentdecl.SyncResult

	existing, err := listDeclaredRemoteNames(ctx, store)
	if err != nil {
		return append(problems, declaredProblem("remote", "could not read existing registrations: "+err.Error()))
	}
	present := map[string]string{}
	for name, id := range existing {
		present[name] = id
	}

	for _, name := range sortedRowNames(desired) {
		row := desired[name]
		if id, ok := present[name]; ok {
			row.ID = id
			if err := store.UpdateRemoteTools(ctx, row); err != nil {
				problems = append(problems, declaredProblem(name, "could not update: "+err.Error()))
			}
			continue
		}
		if err := store.CreateRemoteTools(ctx, row); err != nil {
			problems = append(problems, declaredProblem(name, "could not register: "+err.Error()))
		}
	}

	for name, id := range present {
		if _, kept := desired[name]; kept {
			continue
		}
		if err := store.DeleteRemoteTools(ctx, id); err != nil && !errors.Is(err, libdb.ErrNotFound) {
			problems = append(problems, declaredProblem(name, "could not retire: "+err.Error()))
		}
	}
	return problems
}

func declaredMCPRow(name string, srv agentdecl.DeclaredMCPServer) *runtimetypes.MCPServer {
	return &runtimetypes.MCPServer{
		Name:                  name,
		Transport:             srv.Transport,
		Command:               srv.Command,
		Args:                  srv.Args,
		URL:                   srv.URL,
		Headers:               srv.Headers,
		AuthType:              srv.AuthType,
		AuthEnvKey:            srv.AuthEnvKey,
		ConnectTimeoutSeconds: 30,
	}
}

func declaredRemoteRow(name string, tool agentdecl.DeclaredRemoteTool) *runtimetypes.RemoteTools {
	timeout := tool.TimeoutMs
	if timeout <= 0 {
		timeout = defaultDeclaredRemoteTimeoutMs
	}
	return &runtimetypes.RemoteTools{
		Name:        name,
		EndpointURL: tool.EndpointURL,
		SpecURL:     tool.SpecURL,
		TimeoutMs:   timeout,
		Headers:     tool.Headers,
	}
}

func listDeclaredMCPNames(ctx context.Context, store runtimetypes.Store) ([]string, error) {
	var out []string
	var cursor *time.Time
	for {
		page, err := store.ListMCPServers(ctx, cursor, 100)
		if err != nil {
			return nil, err
		}
		for _, srv := range page {
			if runtimetypes.IsDeclaredToolName(srv.Name) {
				out = append(out, srv.Name)
			}
		}
		if len(page) < 100 {
			return out, nil
		}
		cursor = &page[len(page)-1].CreatedAt
	}
}

func listDeclaredRemoteNames(ctx context.Context, store runtimetypes.Store) (map[string]string, error) {
	out := map[string]string{}
	var cursor *time.Time
	for {
		page, err := store.ListRemoteTools(ctx, cursor, 100)
		if err != nil {
			return nil, err
		}
		for _, tool := range page {
			if runtimetypes.IsDeclaredToolName(tool.Name) {
				out[tool.Name] = tool.ID
			}
		}
		if len(page) < 100 {
			return out, nil
		}
		cursor = &page[len(page)-1].CreatedAt
	}
}

// publishMCPEvent lets the engine's manager start the worker. A nil bus is the
// normal case for a command that only inspects the roster: the row is written,
// and a host that runs agents starts the worker when it comes up.
func publishMCPEvent(ctx context.Context, bus libbus.Messenger, subject string, srv *runtimetypes.MCPServer) {
	if bus == nil {
		return
	}
	data, err := json.Marshal(srv)
	if err != nil {
		return
	}
	_ = bus.Publish(ctx, subject, data)
}

func publishMCPDeleted(ctx context.Context, bus libbus.Messenger, name string) {
	if bus == nil {
		return
	}
	data, err := json.Marshal(mcpworker.MCPDeletedEvent{Name: name})
	if err != nil {
		return
	}
	_ = bus.Publish(ctx, mcpworker.SubjectDeleted, data)
}

func declaredProblem(name, reason string) agentdecl.SyncResult {
	return agentdecl.SyncResult{
		Source: fmt.Sprintf("declared tool source %q", name),
		Name:   name,
		Action: agentdecl.ActionRefused,
		Reason: reason,
	}
}

func sortedRowNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
