package modelregistry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/models/modelregistryservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

var ErrNotFound = errors.New("model not found in registry")

type ModelDescriptor struct {
	ID        string `json:"id,omitempty" example:"m1a2b3c4-d5e6-f7g8-h9i0-j1k2l3m4n5o6"`
	Name      string `json:"name" example:"qwen3-8b"`
	SourceURL string `json:"sourceUrl" example:"https://huggingface.co/Qwen/..."`
	SizeBytes int64  `json:"sizeBytes" example:"934000000"`
	Curated   bool   `json:"curated" example:"true"`
	// Backend is the local backend this model targets: "" or "llama" for GGUF,
	// "openvino" for an OpenVINO IR model. It selects the models/<backend>/
	// directory and the pull strategy (single GGUF file vs multi-file IR repo).
	Backend string `json:"backend,omitempty" example:"openvino"`
	// Repo is the Hugging Face repo id for multi-file pulls (OpenVINO IR). For
	// GGUF models SourceURL points at the single file and Repo is empty.
	Repo string `json:"repo,omitempty" example:"OpenVINO/Qwen3-8B-int4-ov"`
	// ToolProtocol is the backend-native tool-call parser protocol (for example
	// "llama:common_chat_tool_parser" or "openvino:...").
	// Set for curated models certified for tool calls; `model pull` writes it into
	// the model's profile so the local provider enables tool calls out of the box.
	ToolProtocol string `json:"toolProtocol,omitempty" example:"llama:common_chat_tool_parser"`
	// ReasoningProtocol is the local backend's parser for model-emitted reasoning
	// text. `model pull` writes it into the
	// model's profile so the provider can separate visible output from thinking.
	ReasoningProtocol string `json:"reasoningProtocol,omitempty" example:"llama:common_chat_reasoning_parser"`
	// ReasoningFormat is the backend-native reasoning format passed to the parser
	// and chat-template renderer, for example llama.cpp common-chat "deepseek".
	ReasoningFormat string `json:"reasoningFormat,omitempty" example:"deepseek"`
	// Family groups related size/quant variants of the same model line for
	// display (e.g. "qwen3", "gemma4", "phi-4"). It is a presentation grouping
	// key, not a capability signal.
	Family string `json:"family,omitempty" example:"qwen3"`
	// DisplayLabel is the human-readable name shown in listings, e.g.
	// "Qwen 3 8B". Falls back to Name when empty.
	DisplayLabel string `json:"displayLabel,omitempty" example:"Qwen 3 8B"`
	// UseCase is the primary workflow this curated entry is meant to serve,
	// e.g. "coding", "chat", "reasoning", or "smoke".
	UseCase string `json:"useCase,omitempty" example:"coding"`
	// RecommendedVRAMGB is the coarse VRAM tier this curated entry is intended
	// for before live modeld capacity data is available. It is advisory only:
	// modeld still resolves the real hot-KV/effective-context fit from current
	// device free memory, resident policy, KV profile, and runtime overhead.
	RecommendedVRAMGB int `json:"recommendedVramGb,omitempty" example:"8"`
	// Vision marks curated entries whose artifacts carry the full vision stack
	// (multimodal projector beside the GGUF for llama; vision encoder models in
	// the IR snapshot for OpenVINO), so modeld can serve image input. It is the
	// curation-side capability signal only — the live truth stays modeld's
	// Describe answer (ModelInfo.SupportsVision), which the providers thread
	// into CapabilityConfig.CanVision.
	Vision bool `json:"vision,omitempty" example:"true"`
	// MMProjURL is the multimodal projector GGUF for llama vision entries.
	// `model pull` fetches it beside the model as mmproj.gguf — the filename
	// modeld resolves the projector from — and fails loudly when the projector
	// cannot be fetched instead of leaving a silently text-only model. Empty for
	// text entries and for OpenVINO entries (their multi-file snapshot already
	// includes the vision models).
	MMProjURL string `json:"mmprojUrl,omitempty" example:"https://huggingface.co/.../mmproj-....gguf"`
	// MMProjSizeBytes is the projector file size for download reporting.
	MMProjSizeBytes int64 `json:"mmprojSizeBytes,omitempty" example:"559874816"`
	// Notes is a short free-text annotation shown alongside the model in
	// listings, e.g. "native tool format", "MoE", "fastest smoke test".
	Notes string `json:"notes,omitempty" example:"native tool format"`
}

// BackendType returns the local backend this model targets, defaulting empty to
// "llama" for GGUF descriptors.
func (d ModelDescriptor) BackendType() string {
	if d.Backend == "" {
		return "llama"
	}
	return d.Backend
}

// Label returns DisplayLabel, falling back to Name when no display label was set.
func (d ModelDescriptor) Label() string {
	if d.DisplayLabel != "" {
		return d.DisplayLabel
	}
	return d.Name
}

// RecommendedVRAMLabel formats the advisory curated VRAM tier for display.
func (d ModelDescriptor) RecommendedVRAMLabel() string {
	if d.RecommendedVRAMGB <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dGB", d.RecommendedVRAMGB)
}

// residentHeadroomNumerator/Denominator apply a 25% headroom over the on-disk
// weight size, approximating KV cache + runtime overhead at moderate context.
const (
	residentHeadroomNumerator   = 5
	residentHeadroomDenominator = 4
)

// EstimatedResidentBytes is a coarse pre-install RAM/VRAM estimate (on-disk
// weight size — including the multimodal projector for vision entries — plus
// ~25% headroom for KV cache and runtime overhead at a moderate context
// length). It is NOT modeld's real KV-aware capacity.Resolve budget computed
// from the live device and the model's actual KV profile — only a rough
// signal for picking a model before modeld is even installed.
func (d ModelDescriptor) EstimatedResidentBytes() int64 {
	if d.SizeBytes <= 0 {
		return 0
	}
	return (d.SizeBytes + d.MMProjSizeBytes) * residentHeadroomNumerator / residentHeadroomDenominator
}

// FamilyGroup is a display grouping of descriptors sharing the same Family.
type FamilyGroup struct {
	Family  string
	Entries []ModelDescriptor
}

// GroupByFamily groups entries by Family (falling back to Name when Family is
// empty) and sorts groups by family name and entries within a group by
// SizeBytes ascending. It is a pure display helper — it takes no context and
// hits no DB.
func GroupByFamily(entries []ModelDescriptor) []FamilyGroup {
	groups := map[string][]ModelDescriptor{}
	for _, e := range entries {
		key := e.Family
		if key == "" {
			key = e.Name
		}
		groups[key] = append(groups[key], e)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]FamilyGroup, 0, len(keys))
	for _, k := range keys {
		es := groups[k]
		sort.Slice(es, func(i, j int) bool { return es[i].SizeBytes < es[j].SizeBytes })
		out = append(out, FamilyGroup{Family: k, Entries: es})
	}
	return out
}

type Registry interface {
	// Resolve returns the descriptor for name from curated or user-added entries.
	Resolve(ctx context.Context, name string) (*ModelDescriptor, error)
	// List returns all known descriptors (curated + user-added). User entries override curated SourceURL.
	List(ctx context.Context) ([]ModelDescriptor, error)
	// OptimalFor returns the best registry name for an arbitrary model name string.
	// Exact match → family mapping → fallback.
	OptimalFor(ctx context.Context, modelName string) (string, error)
}

type registryImpl struct {
	svc modelregistryservice.Service
}

func New(svc modelregistryservice.Service) Registry {
	return &registryImpl{svc: svc}
}

func (r *registryImpl) Resolve(ctx context.Context, name string) (*ModelDescriptor, error) {
	// User DB entry takes precedence over curated.
	if r.svc != nil {
		if e, err := r.svc.GetByName(ctx, name); err == nil {
			d := mergeUserEntry(e)
			return &d, nil
		} else if !errors.Is(err, libdb.ErrNotFound) {
			return nil, err
		}
	}
	if d, ok := curatedModels[name]; ok {
		return &d, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
}

func (r *registryImpl) List(ctx context.Context) ([]ModelDescriptor, error) {
	merged := make(map[string]ModelDescriptor, len(curatedModels))
	for k, v := range curatedModels {
		merged[k] = v
	}
	if r.svc != nil {
		entries, err := r.svc.List(ctx, nil, 1000)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			merged[e.Name] = mergeUserEntry(e)
		}
	}
	out := make([]ModelDescriptor, 0, len(merged))
	for _, v := range merged {
		out = append(out, v)
	}
	return out, nil
}

func (r *registryImpl) OptimalFor(ctx context.Context, modelName string) (string, error) {
	lower := strings.ToLower(strings.SplitN(modelName, ":", 2)[0])

	// Exact match in curated.
	if _, ok := curatedModels[lower]; ok {
		return lower, nil
	}
	// Exact match in DB.
	if r.svc != nil {
		if _, err := r.svc.GetByName(ctx, lower); err == nil {
			return lower, nil
		}
	}
	// Family substring matching.
	for _, fm := range defaultFamilies {
		for _, sub := range fm.Substrings {
			if strings.Contains(lower, sub) {
				return fm.CanonicalName, nil
			}
		}
	}
	return defaultFallback, nil
}

func mergeUserEntry(e *runtimetypes.ModelRegistryEntry) ModelDescriptor {
	if e == nil {
		return ModelDescriptor{}
	}
	d, ok := curatedModels[e.Name]
	if !ok {
		d = ModelDescriptor{Name: e.Name}
	}
	d.ID = e.ID
	d.Name = e.Name
	d.Curated = false
	if e.SourceURL != "" {
		d.SourceURL = e.SourceURL
	}
	if e.SizeBytes > 0 {
		d.SizeBytes = e.SizeBytes
	}
	return d
}
