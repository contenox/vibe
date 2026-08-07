package enginesvc

import (
	"context"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/llmrepo"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/execservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/mcpworker"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/libtracker"
)

type Config struct {
	DefaultModel       string
	DefaultProvider    string
	AltDefaultModel    string
	AltDefaultProvider string

	// DefaultEmbedModel/DefaultEmbedProvider select the model used for
	// embeddings — often different from the chat model. Empty falls back to
	// the default-embed-model/-provider config keys, then the chat model
	// with a warning (see resolveEmbeddingModel); an unset embedding model
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

	// OnToolsRepoReady, when set, is called exactly once during Build with
	// the aggregate tools repo as a model's own tool call meets it: HITL
	// wrapper included, the outer tool-guidance decorator not (a tool's
	// internal calls aren't model-level navigation). It exists so a tool
	// that itself calls other tools can be registered in LocalTools before
	// the aggregate repo it needs is assembled.
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
