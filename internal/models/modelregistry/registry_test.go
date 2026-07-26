package modelregistry_test

import (
	"context"
	"testing"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/models/modelregistry"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCuratedOnly returns a Registry backed by curated entries only (no DB).
func newCuratedOnly() modelregistry.Registry {
	return modelregistry.New(nil)
}

func TestUnit_Registry_ResolveCuratedByExactName(t *testing.T) {
	reg := newCuratedOnly()
	d, err := reg.Resolve(context.Background(), "qwen3-8b")
	require.NoError(t, err)
	assert.Equal(t, "qwen3-8b", d.Name)
	assert.True(t, d.Curated)
	assert.NotEmpty(t, d.SourceURL)
	assert.Equal(t, "llama:common_chat_tool_parser", d.ToolProtocol)
}

func TestUnit_Registry_ResolveCuratedQwen3CoderGGUF(t *testing.T) {
	reg := newCuratedOnly()
	d, err := reg.Resolve(context.Background(), "qwen3-coder-30b-a3b")
	require.NoError(t, err)

	assert.Equal(t, "qwen3-coder-30b-a3b", d.Name)
	assert.Equal(t, "llama", d.BackendType())
	assert.Equal(t, int64(18_556_689_568), d.SizeBytes)
	assert.Contains(t, d.SourceURL, "unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF")
	assert.Empty(t, d.Repo)
	assert.Equal(t, "llama:common_chat_tool_parser", d.ToolProtocol)
	assert.True(t, d.Curated)
}

func TestUnit_Registry_ResolveCuratedQwen3CoderOpenVINO(t *testing.T) {
	reg := newCuratedOnly()
	d, err := reg.Resolve(context.Background(), "qwen3-coder-30b-a3b-ov")
	require.NoError(t, err)

	assert.Equal(t, "qwen3-coder-30b-a3b-ov", d.Name)
	assert.Equal(t, "openvino", d.BackendType())
	assert.Equal(t, "OpenVINO/Qwen3-Coder-30B-A3B-Instruct-int4-ov", d.Repo)
	assert.Equal(t, int64(16_344_057_522), d.SizeBytes)
	assert.Equal(t, "https://huggingface.co/OpenVINO/Qwen3-Coder-30B-A3B-Instruct-int4-ov", d.SourceURL)
	assert.Equal(t, "openvino:json_schema_tool_calls", d.ToolProtocol)
	assert.True(t, d.Curated)
}

func TestUnit_Registry_ResolveCuratedQwen25CoderOpenVINOTiny(t *testing.T) {
	reg := newCuratedOnly()
	d, err := reg.Resolve(context.Background(), "qwen2.5-coder-0.5b-ov")
	require.NoError(t, err)

	assert.Equal(t, "qwen2.5-coder-0.5b-ov", d.Name)
	assert.Equal(t, "openvino", d.BackendType())
	assert.Equal(t, "OpenVINO/Qwen2.5-Coder-0.5B-Instruct-int4-ov", d.Repo)
	assert.Equal(t, int64(348_761_603), d.SizeBytes)
	assert.Equal(t, "openvino:json_schema_tool_calls", d.ToolProtocol)
	assert.True(t, d.Curated)
}

func TestUnit_Registry_ResolveCuratedQwen25CoderGGUF(t *testing.T) {
	reg := newCuratedOnly()
	d, err := reg.Resolve(context.Background(), "qwen2.5-coder-7b")
	require.NoError(t, err)

	assert.Equal(t, "qwen2.5-coder-7b", d.Name)
	assert.Equal(t, "llama", d.BackendType())
	assert.Equal(t, int64(4_683_073_536), d.SizeBytes)
	assert.Equal(t, "coding", d.UseCase)
	assert.Equal(t, 8, d.RecommendedVRAMGB)
	assert.Contains(t, d.SourceURL, "Qwen/Qwen2.5-Coder-7B-Instruct-GGUF")
	assert.Empty(t, d.Repo)
	assert.True(t, d.Curated)
}

func TestUnit_Registry_ResolveCuratedDevstral(t *testing.T) {
	reg := newCuratedOnly()
	d, err := reg.Resolve(context.Background(), "devstral-small-2507")
	require.NoError(t, err)

	assert.Equal(t, "devstral-small-2507", d.Name)
	assert.Equal(t, "llama", d.BackendType())
	assert.Equal(t, int64(14_333_915_904), d.SizeBytes)
	assert.Equal(t, "coding", d.UseCase)
	assert.Equal(t, 24, d.RecommendedVRAMGB)
	assert.Contains(t, d.SourceURL, "mistralai/Devstral-Small-2507_gguf")
	assert.True(t, d.Curated)
}

func TestUnit_Registry_ResolveCuratedTinyLlamaOpenVINO(t *testing.T) {
	reg := newCuratedOnly()
	d, err := reg.Resolve(context.Background(), "tinyllama-1.1b-chat-v1.0-int4-ov")
	require.NoError(t, err)

	assert.Equal(t, "tinyllama-1.1b-chat-v1.0-int4-ov", d.Name)
	assert.Equal(t, "openvino", d.BackendType())
	assert.Equal(t, "OpenVINO/TinyLlama-1.1B-Chat-v1.0-int4-ov", d.Repo)
	assert.Equal(t, int64(636_000_000), d.SizeBytes)
	assert.Empty(t, d.ToolProtocol)
	assert.True(t, d.Curated)
}

func TestUnit_Registry_ResolveCuratedDeepSeekOpenVINO(t *testing.T) {
	reg := newCuratedOnly()
	d, err := reg.Resolve(context.Background(), "deepseek-r1-distill-qwen-7b-ov")
	require.NoError(t, err)

	assert.Equal(t, "deepseek-r1-distill-qwen-7b-ov", d.Name)
	assert.Equal(t, "openvino", d.BackendType())
	assert.Equal(t, "OpenVINO/DeepSeek-R1-Distill-Qwen-7B-int4-ov", d.Repo)
	assert.Equal(t, int64(4_503_931_427), d.SizeBytes)
	assert.Equal(t, "openvino:json_schema_tool_calls", d.ToolProtocol)
	assert.True(t, d.Curated)
}

func TestUnit_Registry_ListIncludesCurated(t *testing.T) {
	reg := newCuratedOnly()
	entries, err := reg.List(context.Background())
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name] = true
	}
	assert.True(t, names["qwen3-8b"])
	assert.True(t, names["qwen2.5-coder-0.5b-ov"])
	assert.True(t, names["qwen2.5-coder-7b"])
	assert.True(t, names["qwen2.5-coder-14b-ov"])
	assert.True(t, names["starcoder2-7b-instruct"])
	assert.True(t, names["devstral-small-2507"])
	assert.True(t, names["codestral-22b"])
	assert.True(t, names["tinyllama-1.1b-chat-v1.0-int4-ov"])
	assert.True(t, names["qwen3-coder-30b-a3b"])
	assert.True(t, names["qwen3-coder-30b-a3b-ov"])
	assert.True(t, names["gemma4-e4b"])
	assert.False(t, names["gemma3-4b"])
	assert.True(t, names["deepseek-r1-0528-qwen3-8b"])
	assert.True(t, names["gpt-oss-20b"])
	assert.True(t, names["phi-4-mini"])
	assert.True(t, names["granite-3.2-2b"])
	assert.False(t, names["tiny"])
}

func TestUnit_Registry_OptimalForExactCuratedMatch(t *testing.T) {
	reg := newCuratedOnly()
	name, err := reg.OptimalFor(context.Background(), "qwen3-8b")
	require.NoError(t, err)
	assert.Equal(t, "qwen3-8b", name)
}

func TestUnit_Registry_OptimalForFamilyMapping(t *testing.T) {
	reg := newCuratedOnly()
	name, err := reg.OptimalFor(context.Background(), "Qwen3-8B-Instruct")
	require.NoError(t, err)
	assert.Equal(t, "qwen3-8b", name)
}

func TestUnit_Registry_OptimalForQwen3CoderFamilyMapping(t *testing.T) {
	reg := newCuratedOnly()
	name, err := reg.OptimalFor(context.Background(), "Qwen/Qwen3-Coder-30B-A3B-Instruct-GGUF:Q4_K_M")
	require.NoError(t, err)
	assert.Equal(t, "qwen3-coder-30b-a3b", name)
}

func TestUnit_Registry_OptimalForOpenVINOQwen3CoderFamilyMapping(t *testing.T) {
	reg := newCuratedOnly()
	name, err := reg.OptimalFor(context.Background(), "OpenVINO/Qwen3-Coder-30B-A3B-Instruct-int4-ov")
	require.NoError(t, err)
	assert.Equal(t, "qwen3-coder-30b-a3b-ov", name)
}

func TestUnit_Registry_OptimalForCurrentFamilies(t *testing.T) {
	reg := newCuratedOnly()
	tests := map[string]string{
		"google/gemma-4-E4B-it":                        "gemma4-e4b",
		"google/gemma-4-E2B-it":                        "gemma4-e2b",
		"microsoft/Phi-4-mini-instruct":                "phi-4-mini",
		"DeepSeek-R1-0528-Qwen3-8B":                    "deepseek-r1-0528-qwen3-8b",
		"bartowski/openai_gpt-oss-20b":                 "gpt-oss-20b",
		"OpenVINO/gpt-oss-20b-int4-ov":                 "gpt-oss-20b-ov",
		"OpenVINO/gemma-4-E4B-it-int4-ov":              "gemma4-e4b-ov",
		"OpenVINO/Qwen2.5-Coder-0.5B-Instruct-int4-ov": "qwen2.5-coder-0.5b-ov",
		"OpenVINO/Qwen2.5-Coder-14B-Instruct-int4-ov":  "qwen2.5-coder-14b-ov",
		"OpenVINO/TinyLlama-1.1B-Chat-v1.0-int4-ov":    "tinyllama-1.1b-chat-v1.0-int4-ov",
		"Qwen/Qwen2.5-Coder-7B-Instruct-GGUF":          "qwen2.5-coder-7b",
		"mistralai/Devstral-Small-2507_gguf":           "devstral-small-2507",
		"bartowski/Codestral-22B-v0.1-GGUF":            "codestral-22b",
		"bartowski/DeepSeek-Coder-V2-Lite-Instruct":    "deepseek-coder-v2-lite",
		"QuantFactory/starcoder2-7b-instruct-GGUF":     "starcoder2-7b-instruct",
	}
	for input, want := range tests {
		got, err := reg.OptimalFor(context.Background(), input)
		require.NoError(t, err)
		assert.Equal(t, want, got, input)
	}
}

func TestUnit_Registry_OpenVINOCrossSyncWithCuratedLlamaModels(t *testing.T) {
	reg := newCuratedOnly()
	pairs := map[string]string{
		"qwen2.5-coder-0.5b":          "qwen2.5-coder-0.5b-ov",
		"qwen2.5-coder-1.5b":          "qwen2.5-coder-1.5b-ov",
		"qwen2.5-coder-3b":            "qwen2.5-coder-3b-ov",
		"qwen2.5-coder-7b":            "qwen2.5-coder-7b-ov",
		"qwen2.5-coder-14b":           "qwen2.5-coder-14b-ov",
		"phi-4-mini":                  "phi-4-mini-ov",
		"qwen3-4b":                    "qwen3-4b-ov",
		"qwen3-8b":                    "qwen3-8b-ov",
		"qwen3-14b":                   "qwen3-14b-ov",
		"qwen3-30b":                   "qwen3-30b-ov",
		"qwen3-coder-30b-a3b":         "qwen3-coder-30b-a3b-ov",
		"deepseek-r1-distill-qwen-7b": "deepseek-r1-distill-qwen-7b-ov",
		"gpt-oss-20b":                 "gpt-oss-20b-ov",
	}

	for llamaName, openvinoName := range pairs {
		llamaModel, err := reg.Resolve(context.Background(), llamaName)
		require.NoError(t, err, llamaName)
		assert.Equal(t, "llama", llamaModel.BackendType(), llamaName)
		assert.Empty(t, llamaModel.Repo, llamaName)

		openvinoModel, err := reg.Resolve(context.Background(), openvinoName)
		require.NoError(t, err, openvinoName)
		assert.Equal(t, "openvino", openvinoModel.BackendType(), openvinoName)
		assert.NotEmpty(t, openvinoModel.Repo, openvinoName)
		assert.NotEmpty(t, openvinoModel.SourceURL, openvinoName)
		assert.True(t, openvinoModel.Curated, openvinoName)
		if openvinoModel.ToolProtocol != "" {
			assert.Contains(t, openvinoModel.ToolProtocol, "openvino:", openvinoName)
			assert.NotEqual(t, llamaModel.ToolProtocol, openvinoModel.ToolProtocol, openvinoName)
		}
	}
}

// Gemma 4 is multimodal and is curated as a vision model on BOTH local
// backends: the llama entry pairs the model GGUF with its mmproj projector,
// and the OpenVINO entry is the full VLM snapshot served by the GenAI VLM
// pipeline. All URLs/sizes verified live against Hugging Face on 2026-07-22.
func TestUnit_Registry_CuratesGemma4VisionOnBothBackends(t *testing.T) {
	reg := newCuratedOnly()

	llamaModel, err := reg.Resolve(context.Background(), "gemma4-e4b")
	require.NoError(t, err)
	assert.Equal(t, "llama", llamaModel.BackendType())
	assert.True(t, llamaModel.Vision)
	assert.Contains(t, llamaModel.SourceURL, "ggml-org/gemma-4-E4B-it-GGUF")
	assert.Contains(t, llamaModel.MMProjURL, "mmproj-gemma-4-E4B-it")
	assert.Greater(t, llamaModel.MMProjSizeBytes, int64(0))
	assert.Equal(t, 6, llamaModel.RecommendedVRAMGB, "gemma4-e4b vision is the 6GB-tier flagship default")

	twelveB, err := reg.Resolve(context.Background(), "gemma4-12b")
	require.NoError(t, err)
	assert.True(t, twelveB.Vision)
	assert.Contains(t, twelveB.MMProjURL, "mmproj-gemma-4-12B-it")

	ovModel, err := reg.Resolve(context.Background(), "gemma4-e4b-ov")
	require.NoError(t, err)
	assert.Equal(t, "openvino", ovModel.BackendType())
	assert.True(t, ovModel.Vision)
	assert.Equal(t, "OpenVINO/gemma-4-E4B-it-int4-ov", ovModel.Repo)
	assert.Empty(t, ovModel.MMProjURL, "OpenVINO vision entries ship the vision models inside the snapshot")
	assert.Empty(t, ovModel.ToolProtocol, "the OpenVINO VLM pipeline is not certified for tool calls")
	assert.True(t, ovModel.Curated)

	// Smallest vision tier for CPU/iGPU machines where the Gemma 4 snapshot
	// does not fit; verified end-to-end in the OpenVINO VLM cell.
	tiny, err := reg.Resolve(context.Background(), "internvl2-1b-ov")
	require.NoError(t, err)
	assert.Equal(t, "openvino", tiny.BackendType())
	assert.True(t, tiny.Vision)
	assert.Equal(t, "OpenVINO/InternVL2-1B-int4-ov", tiny.Repo)
	assert.Equal(t, 6, tiny.RecommendedVRAMGB)
}

// The multimodal OV repo id must route to the curated OV VLM entry, while the
// generic gemma substrings keep routing text repo ids to the llama entry
// (deliberate collision handling in defaultFamilies ordering).
func TestUnit_Registry_Gemma4FamilySubstringsSplitByBackend(t *testing.T) {
	reg := newCuratedOnly()

	got, err := reg.OptimalFor(context.Background(), "OpenVINO/gemma-4-E4B-it-int4-ov")
	require.NoError(t, err)
	assert.Equal(t, "gemma4-e4b-ov", got)

	got, err = reg.OptimalFor(context.Background(), "google/gemma-4-E4B-it")
	require.NoError(t, err)
	assert.Equal(t, "gemma4-e4b", got)

	got, err = reg.OptimalFor(context.Background(), "gemma")
	require.NoError(t, err)
	assert.Equal(t, "gemma4-e4b", got)
}

// Vision curation invariants: a llama vision entry must record its projector
// (one pull action fetches both artifacts), a non-vision entry must not carry
// one, and OpenVINO entries never need one.
func TestUnit_Registry_VisionEntriesCarryConsistentArtifacts(t *testing.T) {
	reg := newCuratedOnly()
	entries, err := reg.List(context.Background())
	require.NoError(t, err)

	visionCount := 0
	for _, e := range entries {
		switch {
		case e.Vision && e.BackendType() == "llama":
			visionCount++
			assert.NotEmpty(t, e.MMProjURL, "llama vision model %q must record its mmproj URL", e.Name)
			assert.Greater(t, e.MMProjSizeBytes, int64(0), "llama vision model %q must record its mmproj size", e.Name)
		case e.Vision && e.BackendType() == "openvino":
			visionCount++
			assert.NotEmpty(t, e.Repo, "openvino vision model %q must pull the full snapshot", e.Name)
			assert.Empty(t, e.MMProjURL, e.Name)
		default:
			assert.Empty(t, e.MMProjURL, "text model %q must not carry an mmproj URL", e.Name)
			assert.Zero(t, e.MMProjSizeBytes, e.Name)
		}
	}
	assert.GreaterOrEqual(t, visionCount, 3, "expected at least gemma4-e4b, gemma4-12b, and gemma4-e4b-ov vision entries")
}

func TestUnit_Registry_UserEntryKeepsCuratedBackendAndToolProtocol(t *testing.T) {
	svc := fakeModelRegistryService{
		byName: map[string]*runtimetypes.ModelRegistryEntry{
			"qwen3-8b": {
				ID:        "user-qwen3-8b",
				Name:      "qwen3-8b",
				SourceURL: "https://example.com/custom-qwen3-8b.gguf",
			},
		},
	}
	reg := modelregistry.New(svc)

	d, err := reg.Resolve(context.Background(), "qwen3-8b")
	require.NoError(t, err)
	assert.Equal(t, "user-qwen3-8b", d.ID)
	assert.Equal(t, "https://example.com/custom-qwen3-8b.gguf", d.SourceURL)
	assert.Equal(t, int64(5_027_783_488), d.SizeBytes)
	assert.Equal(t, "llama", d.BackendType())
	assert.Equal(t, "llama:common_chat_tool_parser", d.ToolProtocol)
	assert.Equal(t, "llama:common_chat_reasoning_parser", d.ReasoningProtocol)
	assert.Equal(t, "deepseek", d.ReasoningFormat)
	assert.False(t, d.Curated)

	all, err := reg.List(context.Background())
	require.NoError(t, err)
	var listed *modelregistry.ModelDescriptor
	for i := range all {
		if all[i].Name == "qwen3-8b" {
			listed = &all[i]
			break
		}
	}
	require.NotNil(t, listed)
	assert.Equal(t, "llama:common_chat_tool_parser", listed.ToolProtocol)
	assert.Equal(t, "llama:common_chat_reasoning_parser", listed.ReasoningProtocol)
	assert.Equal(t, "deepseek", listed.ReasoningFormat)
	assert.Equal(t, int64(5_027_783_488), listed.SizeBytes)
	assert.False(t, listed.Curated)
}

func TestUnit_Registry_OptimalForFallbackOnUnknown(t *testing.T) {
	reg := newCuratedOnly()
	name, err := reg.OptimalFor(context.Background(), "totally-unknown-model-xyz")
	require.NoError(t, err)
	assert.Equal(t, "qwen3-4b", name) // defaultFallback
}

func TestUnit_Registry_ResolveNotFoundReturnsError(t *testing.T) {
	reg := newCuratedOnly()
	_, err := reg.Resolve(context.Background(), "does-not-exist")
	require.Error(t, err)
	require.ErrorIs(t, err, modelregistry.ErrNotFound)
}

type fakeModelRegistryService struct {
	byName map[string]*runtimetypes.ModelRegistryEntry
}

func (s fakeModelRegistryService) Create(context.Context, *runtimetypes.ModelRegistryEntry) error {
	return nil
}

func (s fakeModelRegistryService) Get(context.Context, string) (*runtimetypes.ModelRegistryEntry, error) {
	return nil, libdb.ErrNotFound
}

func (s fakeModelRegistryService) GetByName(_ context.Context, name string) (*runtimetypes.ModelRegistryEntry, error) {
	if e, ok := s.byName[name]; ok {
		return e, nil
	}
	return nil, libdb.ErrNotFound
}

func (s fakeModelRegistryService) Update(context.Context, *runtimetypes.ModelRegistryEntry) error {
	return nil
}

func (s fakeModelRegistryService) Delete(context.Context, string) error {
	return nil
}

func (s fakeModelRegistryService) List(context.Context, *time.Time, int) ([]*runtimetypes.ModelRegistryEntry, error) {
	out := make([]*runtimetypes.ModelRegistryEntry, 0, len(s.byName))
	for _, e := range s.byName {
		out = append(out, e)
	}
	return out, nil
}

// Every curated entry must carry a Family and a DisplayLabel: these back the
// registry-driven listings in `contenox setup`, `model pull --help`, and
// `model registry-list`. A missing value here would silently degrade those
// listings back to raw registry keys.
func TestUnit_Registry_CuratedEntriesHaveFamilyAndDisplayLabel(t *testing.T) {
	reg := newCuratedOnly()
	entries, err := reg.List(context.Background())
	require.NoError(t, err)

	require.NotEmpty(t, entries)
	for _, e := range entries {
		assert.NotEmpty(t, e.Family, "model %q has no Family", e.Name)
		assert.NotEmpty(t, e.DisplayLabel, "model %q has no DisplayLabel", e.Name)
		assert.NotEmpty(t, e.UseCase, "model %q has no UseCase", e.Name)
		assert.Greater(t, e.RecommendedVRAMGB, 0, "model %q has no RecommendedVRAMGB", e.Name)
		assert.Equal(t, e.DisplayLabel, e.Label(), e.Name)
	}
}

func TestUnit_Registry_LabelFallsBackToNameWhenNoDisplayLabel(t *testing.T) {
	d := modelregistry.ModelDescriptor{Name: "custom-model"}
	assert.Equal(t, "custom-model", d.Label())
}

func TestUnit_Registry_RecommendedVRAMLabel(t *testing.T) {
	d := modelregistry.ModelDescriptor{RecommendedVRAMGB: 16}
	assert.Equal(t, "16GB", d.RecommendedVRAMLabel())

	zero := modelregistry.ModelDescriptor{}
	assert.Equal(t, "-", zero.RecommendedVRAMLabel())
}

func TestUnit_Registry_CodingModelsCoverRecommendedVRAMTiers(t *testing.T) {
	reg := newCuratedOnly()
	entries, err := reg.List(context.Background())
	require.NoError(t, err)

	covered := map[int]bool{}
	for _, e := range entries {
		if e.UseCase == "coding" {
			covered[e.RecommendedVRAMGB] = true
		}
	}
	for _, tier := range []int{6, 8, 16, 24, 32} {
		assert.True(t, covered[tier], "no coding model for %dGB VRAM tier", tier)
	}
}

func TestUnit_Registry_EstimatedResidentBytesAppliesHeadroom(t *testing.T) {
	d := modelregistry.ModelDescriptor{SizeBytes: 4_000_000_000}
	assert.Equal(t, int64(5_000_000_000), d.EstimatedResidentBytes())

	// A vision entry's projector is resident too.
	vision := modelregistry.ModelDescriptor{SizeBytes: 4_000_000_000, MMProjSizeBytes: 400_000_000}
	assert.Equal(t, int64(5_500_000_000), vision.EstimatedResidentBytes())

	zero := modelregistry.ModelDescriptor{}
	assert.Equal(t, int64(0), zero.EstimatedResidentBytes())
}

func TestUnit_Registry_GroupByFamilyGroupsAndSorts(t *testing.T) {
	entries := []modelregistry.ModelDescriptor{
		{Name: "qwen3-14b", Family: "qwen3", SizeBytes: 9_000_000_000},
		{Name: "qwen3-4b", Family: "qwen3", SizeBytes: 2_000_000_000},
		{Name: "qwen3-8b", Family: "qwen3", SizeBytes: 5_000_000_000},
		{Name: "gemma4-e2b", Family: "gemma4", SizeBytes: 4_000_000_000},
		{Name: "standalone-model"}, // no Family: groups under its own Name
	}

	groups := modelregistry.GroupByFamily(entries)
	require.Len(t, groups, 3)

	// Groups sorted alphabetically by family key.
	assert.Equal(t, "gemma4", groups[0].Family)
	assert.Equal(t, "qwen3", groups[1].Family)
	assert.Equal(t, "standalone-model", groups[2].Family)

	// Entries within a group sorted by SizeBytes ascending.
	qwen3 := groups[1].Entries
	require.Len(t, qwen3, 3)
	assert.Equal(t, "qwen3-4b", qwen3[0].Name)
	assert.Equal(t, "qwen3-8b", qwen3[1].Name)
	assert.Equal(t, "qwen3-14b", qwen3[2].Name)
}

func TestUnit_Registry_GroupByFamilyIncludesEveryCuratedModel(t *testing.T) {
	reg := newCuratedOnly()
	entries, err := reg.List(context.Background())
	require.NoError(t, err)

	groups := modelregistry.GroupByFamily(entries)
	total := 0
	for _, g := range groups {
		total += len(g.Entries)
	}
	assert.Equal(t, len(entries), total)
}
