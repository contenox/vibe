// Package transport defines the contract the modeld daemon implements and the
// runtime calls: a persistent, manifest-keyed warm-reuse inference session.
//
// The boundary is the backend-neutral session seam already validated on
// llama.cpp and OpenVINO — EnsurePrefix / PrefillSuffix / Decode. The runtime
// owns this contract; modeld implements it per backend.
//
// An earlier draft put a lower token-level Evaluate/Generate boundary here.
// Stress-checking it against the real llama.Session and OpenVINO GenAISession
// showed both backends sit at this higher, manifest-keyed altitude: OpenVINO
// GenAI holds the tokenizer and chat template internally and caches a string
// prefix, so a token-only daemon could not honor its proven prefix reuse. See
// docs/development/blueprints/modeld/interface-boundary.md.
package transport

import (
	"context"
	"errors"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/contextasm"
)

// ContextManifest is the shared, backend-neutral cache key: profile, model,
// tokenizer/template digests, BOS policy, and stable/volatile hashes. Reuse is
// valid only when the manifest matches; byte equality alone is not enough.
type ContextManifest = contextasm.ContextManifest

// Fence carries the owner identity a client expects to be serving it. It is
// supplied once, at OpenSession; the returned Session is bound to that owner
// epoch, so a takeover invalidates the session rather than every method needing
// a fence. It is a freshness check, not an authentication secret.
type Fence struct {
	OwnerInstanceID string
}

// Config is the explicit hardware/runtime configuration for a session. Every
// knob is a tested setting, not a magic default.
type Config struct {
	NumCtx                  int       // physical engine context window in tokens
	HotContextTokens        int       // physical hot KV budget (0 = NumCtx)
	PlannerEffectiveContext int       // logical planner context window (0 = NumCtx)
	NumBatch                int       // prefill batch size
	NumThreads              int       // CPU threads (0 = NumCPU)
	NumGpuLayers            int       // llama GPU layer cap/resolved count (0 = auto; daemon env may force CPU)
	TensorSplit             []float32 // multi-GPU split
	FlashAttn               bool
	KVCacheType             string // "", "q8_0", "q4_0"

	PromptFormat         string // profile-declared prompt format, e.g. "chatml" or "llama3"
	PromptTemplateDigest string // digest of the declared/rendered prompt template
	DisableBOS           bool
	ReasoningFormat      string // backend-native reasoning format for chat-template parsing/rendering
}

// ModelInfo is what the daemon reports about a model: capabilities resolved from
// the model metadata AND the device's memory by the backend adapter — never
// guessed by the runtime. The runtime is the consumer (capabilities, cache
// identity); it does not parse model files or probe hardware itself.
//
// EffectiveContext is the dense window modeld will actually serve on this
// device today — min(model ceiling, what fits in free memory) — and is the value
// the runtime uses for NumCtx, display, and the cache-identity manifest.
// MemoryContextTokens, HotContextTokens, and PlannerEffectiveContext expose the
// effective-context plumbing separately: raw memory-fit KV tokens, current
// physical hot KV budget, and future logical planner context.
type ModelInfo struct {
	ModelMaxContext         int   `json:"model_max_context"`
	EffectiveContext        int   `json:"effective_context"`
	MemoryContextTokens     int   `json:"memory_context_tokens,omitempty"`
	HotContextTokens        int   `json:"hot_context_tokens,omitempty"`
	PlannerEffectiveContext int   `json:"planner_effective_context,omitempty"`
	KVBytesPerToken         int64 `json:"kv_bytes_per_token,omitempty"`
	FreeBytes               int64 `json:"free_bytes,omitempty"`
	WeightsBytes            int64 `json:"weights_bytes,omitempty"`
	OverheadBytes           int64 `json:"overhead_bytes,omitempty"`
	ReservedBytes           int64 `json:"reserved_bytes,omitempty"`
	UserLimitBytes          int64 `json:"user_limit_bytes,omitempty"`
	MinFreeBytes            int64 `json:"min_free_bytes,omitempty"`
	HostColdBudgetBytes     int64 `json:"host_cold_budget_bytes,omitempty"`
	UsableBytes             int64 `json:"usable_bytes,omitempty"`
	RequiredBytes           int64 `json:"required_bytes,omitempty"`
	Clamped                 bool  `json:"clamped,omitempty"`
	// Reason explains why EffectiveContext was lower than the requested or model
	// dense context. It is telemetry/debug text, not a stable API enum yet.
	Reason string `json:"reason,omitempty"`
	// DeviceKind/DeviceID identify the memory pool modeld used for the capacity
	// decision. Physical hot context is separate from future planner-level
	// effective context, which may exceed the model's dense trained window.
	DeviceKind        string `json:"device_kind,omitempty"`
	DeviceID          string `json:"device_id,omitempty"`
	DeviceTotalBytes  int64  `json:"device_total_bytes,omitempty"`
	SharedWithDisplay bool   `json:"shared_with_display,omitempty"`

	// RequestedGpuLayers is what the profile/env asked for. ResolvedGpuLayers is
	// what modeld will actually open after applying the device memory budget.
	RequestedGpuLayers int `json:"requested_gpu_layers,omitempty"`
	ResolvedGpuLayers  int `json:"resolved_gpu_layers,omitempty"`

	// SparseAttention reports model-native sparse/sliding-window attention that
	// the backend can actually execute for this model. For llama.cpp this means
	// GGUF-declared SWA; it does not mean arbitrary XAttention can be forced on a
	// dense model.
	SparseAttention              bool `json:"sparse_attention,omitempty"`
	SlidingWindowAttentionTokens int  `json:"sliding_window_attention_tokens,omitempty"`

	// SupportsVision reports that the model resolved with a multimodal projector
	// (mmproj) whose metadata declares image input, certified by the backend —
	// never inferred from the model name. VisionTokensPerImage is a conservative
	// planning estimate of the sequence tokens one image consumes after encoding
	// (from projector metadata; 0 = unknown).
	SupportsVision       bool `json:"supports_vision,omitempty"`
	VisionTokensPerImage int  `json:"vision_tokens_per_image,omitempty"`

	// Chat-template capabilities are detected by the backend from the model's own
	// template and parser implementation. The runtime uses these as the default
	// capability truth; registry/profile strings are explicit overrides.
	ChatTemplateFormat                  string `json:"chat_template_format,omitempty"`
	ChatTemplateThinkingStartTag        string `json:"chat_template_thinking_start_tag,omitempty"`
	ChatTemplateReasoningFormat         string `json:"chat_template_reasoning_format,omitempty"`
	ChatTemplateSupportsToolCalls       bool   `json:"chat_template_supports_tool_calls,omitempty"`
	ChatTemplateSupportsThinking        bool   `json:"chat_template_supports_thinking,omitempty"`
	ChatTemplateSupportsReasoningEffort bool   `json:"chat_template_supports_reasoning_effort,omitempty"`

	// Runtime identity and device inventory explain which native runtime modeld
	// actually linked and what memory pools it can allocate from.
	RuntimeName        string       `json:"runtime_name,omitempty"`
	RuntimeDigest      string       `json:"runtime_digest,omitempty"`
	RuntimeSystemInfo  string       `json:"runtime_system_info,omitempty"`
	SupportsGPUOffload bool         `json:"supports_gpu_offload,omitempty"`
	Devices            []DeviceInfo `json:"devices,omitempty"`
}

// ResolvePlannerEffectiveContext normalizes the logical, cold-inclusive planner
// context after a backend has validated the dense hot window. A positive request
// is the caller's desired logical window and may exceed NumCtx; zero accepts the
// daemon-derived planner value. The result is never below NumCtx and is clamped
// to the trained model ceiling when the backend reported one.
func ResolvePlannerEffectiveContext(requested, numCtx int, info ModelInfo) int {
	planner := requested
	if planner <= 0 {
		planner = info.PlannerEffectiveContext
	}
	if planner <= 0 {
		planner = numCtx
	}
	if info.ModelMaxContext > 0 && planner > info.ModelMaxContext {
		planner = info.ModelMaxContext
	}
	if planner < numCtx {
		planner = numCtx
	}
	return planner
}

type DeviceInfo struct {
	Index       int    `json:"index"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	MemoryFree  int64  `json:"memory_free,omitempty"`
	MemoryTotal int64  `json:"memory_total,omitempty"`
	// MemoryFreeKnown/MemoryTotalKnown distinguish absent plugin telemetry from
	// a real zero value.
	MemoryFreeKnown  bool `json:"memory_free_known,omitempty"`
	MemoryTotalKnown bool `json:"memory_total_known,omitempty"`
}

// Service is the entry point modeld serves: it opens persistent sessions on the
// owned hardware, and reports model capabilities it reads from the model itself.
// Opening is where the model is made resident and the session is bound to the
// owner epoch.
type Service interface {
	OpenSession(ctx context.Context, req OpenSessionRequest) (Session, error)
	// Describe reports a model's capabilities from its on-disk metadata. The
	// daemon is the authority because it owns the model format and hardware;
	// Config carries the requested context/runtime knobs for capacity planning.
	Describe(ctx context.Context, req OpenSessionRequest) (ModelInfo, error)
	// Embed computes a one-shot embedding for a model. It uses the same typed
	// handle and owner fence as Describe, but does not create a persistent decode
	// session because embedding pipelines do not participate in KV reuse.
	Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error)
}

// AdapterSpec identifies one LoRA adapter applied to a session: an adapter file
// (Path) applied at Scale, plus Name/Digest carried for cache identity and
// diagnostics. Applying an adapter does not modify the base model weights, but it
// changes model behavior, so a session built with adapters is a different model
// variant than its base. Adapter identity (digest + scale, in order) is therefore
// part of every session, manifest, and slot cache key — warm KV for base+A must
// never be reused for base+B (see docs/development/blueprints/modeld/lora-adapters.md). The
// backend adapters (modeld/llama.AdapterSpec, ovsession.GenAILoRAAdapter) mirror
// this shape; Scale maps to the llama scale and to OpenVINO's folded alpha.
type AdapterSpec struct {
	Name   string  `json:"name,omitempty"`
	Path   string  `json:"path,omitempty"`
	Digest string  `json:"digest,omitempty"`
	Scale  float32 `json:"scale,omitempty"`
}

// OpenSessionRequest asks the owner to open a session for a model. The model is
// identified by a typed handle, not an opaque path: ModelName + Type + Digest is
// the cache identity, and Type lets the daemon reject a model it does not serve
// (see ErrBackendMismatch) instead of failing deep in the engine. Path is the
// runtime-resolved on-disk location the daemon loads from — a hint, not identity.
type OpenSessionRequest struct {
	Fence
	ModelName string // logical model name, e.g. "qwen2.5-1.5b"
	Type      string // backend type the model targets: "llama" | "openvino"
	Digest    string // content digest; part of the cache identity
	Path      string // runtime-resolved filesystem location (GGUF file or IR dir)
	Config    Config
	// Adapters are LoRA adapters applied to this session, in order. Empty = the
	// base model. They are part of the cache identity, not a Config knob.
	Adapters []AdapterSpec `json:"adapters,omitempty"`
	// ReclaimableBytes is a Describe-only hint set by the slot owner: bytes it
	// estimates would be freed if answering this request required evicting the
	// currently resident session. Backends credit it to the free-memory
	// snapshot so Describe answers "what would I get if I opened this now"
	// instead of under-reporting against memory the switch itself would
	// reclaim. OpenSession/LoadModel ignore it (they describe after the real
	// close). Deliberately top-level, not a Config field: it must never enter
	// session cache identity or config equality.
	ReclaimableBytes int64 `json:"reclaimable_bytes,omitempty"`
}

// EmbedRequest asks the owner to compute a one-shot embedding for Text.
type EmbedRequest struct {
	Fence
	ModelName string // logical model name, e.g. "bge-small-en"
	Type      string // backend type the model targets: "llama" | "openvino"
	Digest    string // content digest; part of the model identity
	Path      string // runtime-resolved filesystem location (GGUF file or IR dir)
	Config    Config
	Text      string
}

type EmbedResult struct {
	Vector []float32 `json:"vector,omitempty"`
}

// SlotState is the daemon-visible lifecycle state of the single active local
// model slot. The empty string is treated as SlotEmpty by older callers.
type SlotState string

const (
	SlotEmpty        SlotState = "Empty"
	SlotLoading      SlotState = "Loading"
	SlotReady        SlotState = "Ready"
	SlotBusy         SlotState = "Busy"
	SlotSwitching    SlotState = "Switching"
	SlotUnloading    SlotState = "Unloading"
	SlotFailed       SlotState = "Failed"
	SlotShuttingDown SlotState = "ShuttingDown"
	SlotLostOwner    SlotState = "LostOwner"
	// SlotIdle means the daemon released a resident model after it sat idle past
	// the configured TTL, freeing device memory so the GPU can return to idle. The
	// next request reopens the slot transparently (the client's stale handle gets
	// ErrSlotGenerationStale and reopens).
	SlotIdle SlotState = "Idle"
)

// ActiveModel describes the model identity and runtime config currently loaded
// in modeld's single active slot.
type ActiveModel struct {
	ModelName  string        `json:"model_name,omitempty"`
	Type       string        `json:"type,omitempty"`
	Digest     string        `json:"digest,omitempty"`
	Path       string        `json:"path,omitempty"`
	Config     Config        `json:"config"`
	Adapters   []AdapterSpec `json:"adapters,omitempty"`
	Generation uint64        `json:"generation"`
}

// DaemonStatus reports the owner-local modeld slot state. It is intentionally
// about resident compute state, not the offline installed-model library.
type DaemonStatus struct {
	OwnerInstanceID string       `json:"owner_instance_id,omitempty"`
	Backend         string       `json:"backend,omitempty"`
	State           SlotState    `json:"state,omitempty"`
	Active          *ActiveModel `json:"active,omitempty"`
	BusyOperation   string       `json:"busy_operation,omitempty"`
	LastError       string       `json:"last_error,omitempty"`
}

// LoadModelRequest explicitly activates modeld's single local model slot. A
// different active model may be switched only when the slot has no open session
// holder and is not busy.
type LoadModelRequest struct {
	Fence
	ModelName string
	Type      string
	Digest    string
	Path      string
	Config    Config
	// Adapters are LoRA adapters applied to this model variant, in order. Empty =
	// the base model. Part of the slot's model identity.
	Adapters []AdapterSpec `json:"adapters,omitempty"`
	// ExpectedGeneration, when non-zero, makes load/switch conditional on the
	// caller's view of the active slot.
	ExpectedGeneration uint64
}

// UnloadModelRequest explicitly releases modeld's active slot. It is idempotent
// when the slot is already empty unless ExpectedGeneration is set and stale.
type UnloadModelRequest struct {
	Fence
	ExpectedGeneration uint64
}

// ModelController is implemented by modeld services that expose explicit
// single-slot control. Service remains the compute contract; this interface is
// the daemon lifecycle/control extension.
type ModelController interface {
	Status(ctx context.Context) (DaemonStatus, error)
	LoadModel(ctx context.Context, req LoadModelRequest) (ActiveModel, error)
	UnloadModel(ctx context.Context, req UnloadModelRequest) error
}

// Session is a persistent, workspace-scoped inference session. The hot coding
// loop is EnsurePrefix -> PrefillSuffix -> Decode: keep the stable prefix's KV
// hot, re-prefill only the changed suffix, decode.
type Session interface {
	// EnsurePrefix makes the resident KV equal `prefix`, reusing the longest
	// already-resident matching prefix and prefilling only the divergent tail
	// (this also drops any previous suffix and generated tokens).
	EnsurePrefix(ctx context.Context, prefix PrefixInput) (PrefixStatus, error)

	// PrefillSuffix prefills the volatile suffix (diff / test output / user
	// turn) after the stable prefix, leaving the stable KV untouched.
	PrefillSuffix(ctx context.Context, suffix SuffixInput) (SuffixStatus, error)

	// Decode streams generated text from the current resident state.
	Decode(ctx context.Context, cfg DecodeConfig) (<-chan StreamChunk, error)

	// ExplainContext reports the resident context for observability.
	ExplainContext() ContextReport

	// Snapshot captures backend state for durability/branching. State is opaque
	// backend data; the manifest and bookkeeping fields are the compatibility
	// gate needed before Restore may trust it.
	Snapshot(ctx context.Context) (SessionSnapshot, error)

	// Restore replaces the resident session state from a compatible snapshot.
	Restore(ctx context.Context, snap SessionSnapshot) error

	// Close releases the session's resources.
	Close() error
}

type SessionSnapshot struct {
	StateID          string          `json:"state_id,omitempty"`
	State            []byte          `json:"-"`
	ResidentTokens   int             `json:"resident_tokens,omitempty"`
	PrefixTokens     int             `json:"prefix_tokens,omitempty"`
	NumCtx           int             `json:"num_ctx,omitempty"`
	ResidentTokenIDs []int           `json:"resident_token_ids,omitempty"`
	ColdKVBlocks     []ColdKVBlock   `json:"cold_kv_blocks,omitempty"`
	StableText       string          `json:"stable_text,omitempty"`
	PrefixText       string          `json:"prefix_text,omitempty"`
	Tools            string          `json:"tools,omitempty"`
	Manifest         ContextManifest `json:"manifest"`
}

// ColdKVBlock is an opaque host-resident KV block captured alongside a session
// snapshot. Start/End are logical token positions from the context at eviction
// time; a backend may restore the block at a different hot position if it can
// perform the required position fixups.
type ColdKVBlock struct {
	Start        int    `json:"start,omitempty"`
	End          int    `json:"end,omitempty"`
	Tokens       []int  `json:"tokens,omitempty"`
	PrefixTokens []int  `json:"prefix_tokens,omitempty"`
	TokenHash    string `json:"token_hash,omitempty"`
	KV           []byte `json:"kv,omitempty"`
	CacheClass   string `json:"cache_class,omitempty"`
	LastUsed     int64  `json:"last_used,omitempty"`
}

// MediaMarker is the model-agnostic placeholder a caller embeds in input text
// where an attached image belongs — one marker per ImagePart, in reading order.
// The backend replaces each marker with the model's native media tokens; the
// marker string itself never reaches the model. The value is fixed by the
// multimodal engine (llama.cpp mtmd's default marker) and identical for every
// model, which is what lets the runtime place it without backend knowledge.
const MediaMarker = "<__media__>"

// ImagePart is one encoded image attached to a session input: the raw bytes of
// an image file (PNG/JPEG/BMP/...), decoded natively by the backend. MimeType
// is advisory; backends sniff the actual format from the bytes.
type ImagePart struct {
	Data     []byte `json:"data,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// PrefixInput is the stable prefix text plus the manifest that makes reuse
// valid: tokenizer, template, runtime config, BOS policy, and model identity are
// part of the cache key, not just the text.
type PrefixInput struct {
	Text     string
	Manifest ContextManifest
	// Tools is a JSON array of tool definitions to render into the prompt via the
	// model's own GGUF chat template (model-native tool calls). "" means no tools.
	// The daemon renders it; the runtime never sees the model's tool format.
	Tools string `json:",omitempty"`
	// Images are the image parts referenced by MediaMarker occurrences in Text,
	// in order. Backends without projector support — and the current llama
	// session, which keys prefix reuse on a token-only tape — reject a stable
	// prefix carrying images; attach images to the volatile suffix instead.
	Images []ImagePart `json:"images,omitempty"`
}

// SuffixInput is the volatile text appended after the stable prefix. It carries
// the same manifest so a suffix cannot be prefilled against resident KV from a
// different profile/template/runtime.
type SuffixInput struct {
	Text     string
	Manifest ContextManifest
	// EnableThinking controls model-owned chat-template rendering for the
	// assistant generation prompt when a backend supports it. nil means backend
	// default.
	EnableThinking *bool `json:",omitempty"`
	// ReasoningEffort is passed to templates that consume a reasoning_effort
	// kwarg (for example harmony/gpt-oss). Empty means backend default.
	ReasoningEffort string `json:",omitempty"`
	// Images are the image parts referenced by MediaMarker occurrences in Text,
	// in order (all markers across the volatile conversation, so a multi-turn
	// resend keeps earlier images evaluable). Requires a model resolved with
	// vision support (ModelInfo.SupportsVision).
	Images []ImagePart `json:"images,omitempty"`
}

// PrefixStatus reports what EnsurePrefix reused versus had to (re)compute.
// ReusedTokens > 0 is a warm hit.
type PrefixStatus struct {
	ReusedTokens    int
	PrefilledTokens int
	DroppedTokens   int
	PrefixTokens    int
	ResidentTokens  int
	AvailableTokens int
	StableByteHash  string
	StableTokenHash string
	ManifestDigest  string
}

// SuffixStatus reports the volatile suffix added after the stable prefix.
type SuffixStatus struct {
	SuffixTokens    int
	PrefixTokens    int
	ResidentTokens  int
	AvailableTokens int
	ManifestDigest  string
}

// DecodeConfig controls a single decode pass.
type DecodeConfig struct {
	MaxTokens        int
	Temperature      *float64
	TopP             *float64
	TopK             int
	Seed             *int
	ParserProtocols  []string
	ReasoningFormat  string
	StructuredOutput StructuredOutputConfig
}

// StructuredOutputConfig carries a backend-typed structured-output request for
// decode calls that need the native backend to constrain generation.
type StructuredOutputConfig struct {
	Protocol string
	Payload  string
	ToolName string
}

// ToolCall is a backend-neutral parsed function call emitted by a model.
type ToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// StreamChunk is a decoded text delta, parsed model output, or a terminal error.
type StreamChunk struct {
	Text      string
	Thinking  string
	ToolCalls []ToolCall
	Error     error
}

// ContextReport explains the session's resident context (explain-context).
type ContextReport struct {
	ResidentTokens          int
	PrefixTokens            int
	NumCtx                  int
	HotContextTokens        int
	PlannerEffectiveContext int
	AvailableTokens         int
	StableByteHash          string
	StableTokenHash         string
	ManifestDigest          string
	Manifest                ContextManifest
	Closed                  bool
	FatalError              string `json:"fatal_error,omitempty"`
	PhysicalPrefillCalls    int    `json:"physical_prefill_calls,omitempty"`
	PhysicalPrefillTokens   int    `json:"physical_prefill_tokens,omitempty"`
	DeferredPrefillCalls    int    `json:"deferred_prefill_calls,omitempty"`
	DeferredPrefillTokens   int    `json:"deferred_prefill_tokens,omitempty"`
	DecodeCalls             int    `json:"decode_calls,omitempty"`
	DecodePromptTokens      int    `json:"decode_prompt_tokens,omitempty"`
	// Residency is the backend's current KV residency plan: the hot/cold
	// partition it would apply under the derived hot budget. It is observability;
	// the plan is enforced only when the backend can execute it (Capabilities).
	// Nil when the backend computes no plan.
	Residency *ResidencyReport `json:"residency,omitempty"`
}

// ResidencyCapabilities mirrors what a backend can physically execute against
// resident KV. The runtime reads it to know which residency plans are actionable
// on the serving backend versus observational only.
type ResidencyCapabilities struct {
	RemoveTail                   bool `json:"remove_tail,omitempty"`
	RemoveMiddle                 bool `json:"remove_middle,omitempty"`
	PositionShift                bool `json:"position_shift,omitempty"`
	SparseAttention              bool `json:"sparse_attention,omitempty"`
	SlidingWindowAttentionTokens int  `json:"sliding_window_attention_tokens,omitempty"`
	ColdStore                    bool `json:"cold_store,omitempty"`
	RecomputeRange               bool `json:"recompute_range,omitempty"`
}

// ResidencyReport explains a backend's KV residency plan for observability: the
// hot/cold partition under the derived hot budget, what is protected, and what
// the backend could execute. It is backend-neutral; the modeld residency planner
// produces it and the adapter maps it onto this type.
type ResidencyReport struct {
	BudgetTokens    int                   `json:"budget_tokens,omitempty"`
	TotalTokens     int                   `json:"total_tokens,omitempty"`
	HotTokens       int                   `json:"hot_tokens,omitempty"`
	ColdTokens      int                   `json:"cold_tokens,omitempty"`
	ProtectedTokens int                   `json:"protected_tokens,omitempty"`
	HotBlocks       int                   `json:"hot_blocks,omitempty"`
	ColdBlocks      int                   `json:"cold_blocks,omitempty"`
	OverBudget      bool                  `json:"over_budget,omitempty"`
	Capabilities    ResidencyCapabilities `json:"capabilities"`
	Diagnostics     []string              `json:"diagnostics,omitempty"`
	Error           string                `json:"error,omitempty"`
}

// Canonical errors expected to cross the boundary.
var (
	ErrNotOwner        = errors.New("instance is not the local runtime owner")
	ErrStaleFence      = errors.New("stale owner fence token")
	ErrSessionClosed   = errors.New("session is closed")
	ErrContextOverflow = errors.New("exceeded the session context window")
	// ErrSessionFatal means the backend marked the session unusable; the client
	// must evict the session and reopen instead of reusing resident state.
	ErrSessionFatal        = errors.New("session marked fatal; evict and reopen")
	ErrModelBusy           = errors.New("modeld active model slot is busy")
	ErrModelNotActive      = errors.New("requested model is not active in modeld")
	ErrModelSwitchRequired = errors.New("modeld active model slot must be switched")
	ErrModelLoadFailed     = errors.New("modeld failed to load model")
	ErrInsufficientMemory  = errors.New("insufficient memory for requested model")
	ErrDeviceBusy          = errors.New("modeld accelerator device is busy")
	ErrSlotGenerationStale = errors.New("stale modeld slot generation")
	// ErrBackendMismatch means the requested model Type is not the backend this
	// daemon serves (e.g. a llama model requested from an openvino-mode modeld).
	ErrBackendMismatch = errors.New("model type not served by this modeld backend")
	// ErrUnsupportedFeature means the backend is healthy but does not implement
	// the requested product surface for this model or backend mode.
	ErrUnsupportedFeature = errors.New("unsupported transport feature")
	// ErrModelNotFound means the request omitted an explicit Path and the
	// daemon could not resolve ModelName+Type against its own models
	// directory (see modeld/modelstore). Remote nodes cannot use a
	// caller-supplied filesystem path, so they always resolve locally.
	ErrModelNotFound = errors.New("model not found in modeld's local store")
	// ErrDigestMismatch means content resolved or received by the daemon
	// (an on-disk model file, or a pushed model stream) does not hash to a
	// digest the caller asserted.
	ErrDigestMismatch = errors.New("model content digest mismatch")
	// ErrUnsupportedModelType means the daemon does not know how to resolve,
	// store, or push a model of the given backend type (only "llama" and
	// "openvino" are recognized).
	ErrUnsupportedModelType = errors.New("unsupported model backend type")
)

// ContextOverflowDetail carries the structured token counts for a context-window
// overflow. It is intentionally backend-neutral so the gRPC boundary can preserve
// these fields without knowing whether llama.cpp, OpenVINO, or a future backend
// produced the error.
type ContextOverflowDetail struct {
	Stage            string `json:"stage,omitempty"`
	ResidentTokens   int    `json:"resident_tokens,omitempty"`
	AdditionalTokens int    `json:"additional_tokens,omitempty"`
	NumCtx           int    `json:"num_ctx,omitempty"`
}

// ContextOverflowError is the typed transport error for ErrContextOverflow.
type ContextOverflowError struct {
	ContextOverflowDetail
}

func (e *ContextOverflowError) Error() string {
	return fmt.Sprintf("%s during %s: resident_tokens=%d additional_tokens=%d num_ctx=%d",
		ErrContextOverflow, e.Stage, e.ResidentTokens, e.AdditionalTokens, e.NumCtx)
}

func (e *ContextOverflowError) Is(target error) bool {
	return target == ErrContextOverflow
}

func (e *ContextOverflowError) OverflowDetail() ContextOverflowDetail {
	if e == nil {
		return ContextOverflowDetail{}
	}
	return e.ContextOverflowDetail
}

func NewContextOverflowError(stage string, resident, additional, numCtx int) error {
	return &ContextOverflowError{
		ContextOverflowDetail: ContextOverflowDetail{
			Stage:            stage,
			ResidentTokens:   resident,
			AdditionalTokens: additional,
			NumCtx:           numCtx,
		},
	}
}

type overflowDetailer interface {
	OverflowDetail() ContextOverflowDetail
}

func ContextOverflowDetailFromError(err error) (ContextOverflowDetail, bool) {
	var detailer overflowDetailer
	if errors.As(err, &detailer) {
		detail := detailer.OverflowDetail()
		if detail.Stage != "" || detail.ResidentTokens != 0 || detail.AdditionalTokens != 0 || detail.NumCtx != 0 {
			return detail, true
		}
	}
	return ContextOverflowDetail{}, false
}
