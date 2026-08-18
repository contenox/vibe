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

	// DefaultAudioModel/DefaultAudioProvider select the model preferred for
	// audio-bearing requests; empty falls back to the default-audio-model/
	// -provider config keys.
	DefaultAudioModel    string
	DefaultAudioProvider string

	// ReadinessDefaultModel/ReadinessDefaultProvider are effective defaults
	// credited during readiness evaluation when the persisted KV config leaves
	// them unset; empty means no override.
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
	// State is the runtime backend/model snapshot to use; when nil, Build
	// creates one.
	State           *runtimestate.State
	Tracker         libtracker.ActivityTracker
	ExtraInspectors []func(taskengine.Inspector) taskengine.Inspector
	TaskEventSink   taskengine.TaskEventSink

	Tracing bool

	SkipBackendCycle bool

	WorkspaceID string
	// TenantID is the tenant the engine operates under; empty defaults to
	// runtimetypes.LocalTenantID.
	TenantID string
	// HITLPolicySource supplies HITL policy documents, used only when
	// EnableHITL is set and HITLService is nil.
	HITLPolicySource hitlservice.PolicySource

	// OnToolsRepoReady, when set, is called exactly once during Build with the
	// aggregate tools repo (HITL wrapper included, tool-guidance decorator
	// not).
	OnToolsRepoReady func(taskengine.ToolsRepo)
}

type Engine struct {
	TaskService execservice.TasksEnvService
	// Models is the shared model repo the engine built — the ONE resolved
	// route to a provider.
	Models llmrepo.ModelRepo
	// AudioModel is the model/provider role preferred for audio-bearing
	// requests, already resolved through config keys; zero-valued when unset.
	AudioModel llmrepo.ModelConfig
	Tracker    libtracker.ActivityTracker
	Bus        libbus.Messenger
	State      *runtimestate.State
	MCPManager *mcpworker.Manager
	LocalTools []string
	// Tools is the aggregate repo every turn resolves tools through: the
	// LocalTools sets plus store-registered MCP servers and remote providers,
	// wraps included. Read-only surfaces (/doctor) must enumerate here so
	// their report cannot drift from what a turn advertises.
	Tools         taskengine.ToolsRepo
	SetupCheck    setupcheck.Result
	TaskEventSink taskengine.TaskEventSink
	Stop          func()
	// SetupStatus recomputes current readiness from live runtime state,
	// read-only; SetupCheck above is the build-time snapshot.
	SetupStatus func(ctx context.Context) (setupcheck.Result, error)
}
