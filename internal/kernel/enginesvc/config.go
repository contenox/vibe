package enginesvc

import (
	"context"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libbus"
	"github.com/contenox/beam/internal/libkvstore"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/llmrepo"
	"github.com/contenox/beam/internal/models/runtimestate"
	"github.com/contenox/beam/internal/services/execservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/mcpworker"
	"github.com/contenox/beam/internal/services/setupcheck"
)

type Config struct {
	DefaultModel       string
	DefaultProvider    string
	AltDefaultModel    string
	AltDefaultProvider string

	// DefaultEmbedModel/DefaultEmbedProvider select the model used for
	// EMBEDDINGS — a different model from the chat one on most providers. Empty
	// means "read the default-embed-model / default-embed-provider config keys,
	// then fall back to the chat model with a warning" (see
	// resolveEmbeddingModel). Retrieval is optional, so an unset embedding model
	// never fails Build.
	DefaultEmbedModel    string
	DefaultEmbedProvider string

	// ReadinessDefaultModel/ReadinessDefaultProvider are effective defaults to
	// credit during readiness evaluation when the persisted KV config leaves them
	// unset — e.g. the CLI's --model/--provider flags, which configure a single
	// invocation without writing config. Empty means "no override"; server/ACP/
	// editor paths leave these blank and rely solely on persisted config.
	ReadinessDefaultModel    string
	ReadinessDefaultProvider string

	ContextLength int

	NoDeleteModels bool

	LocalTools map[string]taskengine.ToolsRepo

	EnableHITL            bool
	AskApproval           localtools.AskApproval
	HITLService           hitlservice.Service
	HITLDefaultPolicyName string

	Bus     libbus.Messenger
	KVStore libkvstore.KVManager
	// State is the runtime backend/model snapshot to use. When nil, Build
	// creates one. Supplying it lets HTTP routes and the engine share exactly
	// one observed runtime state.
	State           *runtimestate.State
	Tracker         libtracker.ActivityTracker
	ExtraInspectors []func(taskengine.Inspector) taskengine.Inspector
	TaskEventSink   taskengine.TaskEventSink

	Tracing bool

	SkipBackendCycle bool

	WorkspaceID string
	// TenantID is the tenant the engine operates under. When empty, defaults
	// to runtimetypes.LocalTenantID. Multi-tenant embedders pass real tenant IDs.
	TenantID string
	// HITLPolicySource supplies HITL policy documents (used only when EnableHITL
	// is set and HITLService is nil). The default is a filesystem-backed source;
	// embedders can inject their own.
	HITLPolicySource hitlservice.PolicySource

	// OnToolsRepoReady, when set, is called exactly once during Build with the
	// aggregate tools repo AS A MODEL'S OWN TOOL CALL MEETS IT — HITL wrapper
	// included, the outer attention/tool-guidance decorator NOT.
	//
	// It exists for one unavoidable construction cycle: a tool that itself calls
	// other tools has to be registered in LocalTools BEFORE the aggregate repo
	// those tools are assembled into exists, yet it needs that aggregate to do its
	// job. Late binding through this callback is the smallest honest seam, and it
	// keeps the knowledge one-directional — the composition root knows who needs
	// the repo; enginesvc only knows when the repo is ready.
	//
	// Both halves of "which repo" are deliberate. HITL-wrapped, because gating is
	// the boundary a nested caller must not slip past: a tool reaching another
	// tool must meet the same envelope the model would. NOT guidance-wrapped,
	// because that decorator counts MODEL-level navigation, and a tool's internal
	// calls are not that — feeding them in would make the counters measure
	// something else.
	OnToolsRepoReady func(taskengine.ToolsRepo)
}

type Engine struct {
	TaskService execservice.TasksEnvService
	// Models is the shared model repo the engine built — the ONE resolved
	// route to a provider. Exposed so a caller that needs a model seam the task
	// engine does not carry (embeddings, for the workspace index) composes this
	// one rather than standing up a second model manager beside it.
	Models llmrepo.ModelRepo
	// EmbeddingModel is the model/provider Models embeds with, already resolved
	// through config keys and the chat-model fallback (resolveEmbeddingModel).
	// A caller recording which model produced a set of vectors reads it here.
	EmbeddingModel llmrepo.ModelConfig
	Tracker        libtracker.ActivityTracker
	Bus            libbus.Messenger
	State          *runtimestate.State
	MCPManager     *mcpworker.Manager
	LocalTools     []string
	SetupCheck     setupcheck.Result
	TaskEventSink  taskengine.TaskEventSink
	Stop           func()
	// SetupStatus recomputes current readiness from live runtime state (read-only:
	// reads synced backend state + config, never probes or runs a completion).
	// SetupCheck above is the build-time snapshot; this reflects the latest state.
	SetupStatus func(ctx context.Context) (setupcheck.Result, error)
}
