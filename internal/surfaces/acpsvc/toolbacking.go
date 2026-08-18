package acpsvc

import (
	"context"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/getkin/kin-openapi/openapi3"
)

// ClientBackedToolset wraps a toolset whose only implementation proxies to the
// ACP client, so a session is advertised just the tools that connection can
// serve. Registration is unchanged and Exec still refuses at the backing; no
// attached client advertises nothing.
func ClientBackedToolset(repo taskengine.ToolsRepo, transport TransportResolver) taskengine.ToolsRepo {
	return &clientBackedRepo{inner: repo, transport: transport}
}

type clientBackedRepo struct {
	inner     taskengine.ToolsRepo
	transport TransportResolver
}

func (r *clientBackedRepo) clientCaps(ctx context.Context) libacp.ClientCapabilities {
	if r.transport == nil {
		return libacp.ClientCapabilities{}
	}
	t := r.transport(ctx)
	if t == nil {
		return libacp.ClientCapabilities{}
	}
	return t.getClientCaps()
}

func (r *clientBackedRepo) Exec(ctx context.Context, startingTime time.Time, input any, debug bool, args *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	return r.inner.Exec(ctx, startingTime, input, debug, args)
}

// Precheck forwards to the proxied toolset so a call it will refuse anyway is
// refused before the gate asks a human.
func (r *clientBackedRepo) Precheck(ctx context.Context, input any, args *taskengine.ToolsCall) error {
	pre, ok := r.inner.(taskengine.Prechecker)
	if !ok {
		return nil
	}
	return pre.Precheck(ctx, input, args)
}

func (r *clientBackedRepo) Supports(ctx context.Context) ([]string, error) {
	return r.inner.Supports(ctx)
}

func (r *clientBackedRepo) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	return r.inner.GetSchemasForSupportedTools(ctx)
}

func (r *clientBackedRepo) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	tools, err := r.inner.GetToolsForToolsByName(ctx, name)
	if err != nil {
		return nil, err
	}
	return filterToolsForCaps(name, tools, r.clientCaps(ctx)), nil
}

var (
	_ taskengine.ToolsRepo  = (*clientBackedRepo)(nil)
	_ taskengine.Prechecker = (*clientBackedRepo)(nil)
)

// IsClientBacked reports whether repo is a ClientBackedToolset: every tool it
// serves proxies to the attached ACP client.
func IsClientBacked(repo taskengine.ToolsRepo) bool {
	_, ok := repo.(*clientBackedRepo)
	return ok
}

// PotentialClientTools is repo's tool list before capability filtering: what a
// ClientBackedToolset would advertise to a fully capable client. A non-wrapped
// repo reports its advertised list unchanged. For clientless surfaces (doctor)
// that must name what a session could hold.
func PotentialClientTools(ctx context.Context, repo taskengine.ToolsRepo, name string) ([]taskengine.Tool, error) {
	if cb, ok := repo.(*clientBackedRepo); ok {
		return cb.inner.GetToolsForToolsByName(ctx, name)
	}
	return repo.GetToolsForToolsByName(ctx, name)
}

// RequiredClientCapability names the client capability that alone backs one
// tool of a client-proxied toolset; "" means the tool runs in-process. It must
// mirror clientCanServe — TestUnit_RequiredClientCapability_MirrorsClientCanServe
// pins the pairing.
func RequiredClientCapability(toolsName, toolName string) string {
	switch toolsName {
	case localtools.LocalFSToolsName:
		switch toolName {
		case "read_file", "read_file_range":
			return "fs.readTextFile"
		default:
			return "fs.readTextFile+fs.writeTextFile"
		}
	case localtools.LocalExecToolsName:
		return "terminal"
	}
	return ""
}

// clientProxiedCapability names the capability family backing an entire
// client-proxied toolset ("" for an in-process one); per-tool detail comes
// from RequiredClientCapability.
func clientProxiedCapability(toolsName string) string {
	switch toolsName {
	case localtools.LocalFSToolsName:
		return "fs.readTextFile/fs.writeTextFile"
	case localtools.LocalExecToolsName:
		return "terminal"
	}
	return ""
}

// filterToolsForCaps drops the tools of a client-proxied toolset that the
// attached client's advertised capabilities cannot back.
func filterToolsForCaps(toolsName string, tools []taskengine.Tool, caps libacp.ClientCapabilities) []taskengine.Tool {
	kept := make([]taskengine.Tool, 0, len(tools))
	for _, tool := range tools {
		if !clientCanServe(toolsName, tool.Function.Name, caps) {
			continue
		}
		kept = append(kept, tool)
	}
	return kept
}

// clientCanServe reports whether caps back one tool's only implementation. A
// local_fs mutation needs read as well as write: requireReadBeforeMutation reads
// the current file through the same client before every write to an existing one.
func clientCanServe(toolsName, toolName string, caps libacp.ClientCapabilities) bool {
	switch toolsName {
	case localtools.LocalFSToolsName:
		switch toolName {
		case "read_file", "read_file_range":
			return caps.FS.ReadTextFile
		default:
			return caps.FS.ReadTextFile && caps.FS.WriteTextFile
		}
	case localtools.LocalExecToolsName:
		return caps.Terminal
	}
	return true
}
