package llama

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/transport"
)

type staticMemory int64

func (m staticMemory) FreeBytes() (int64, error) { return int64(m), nil }

type staticSnapshot struct {
	snap capacity.DeviceSnapshot
}

func (s staticSnapshot) FreeBytes() (int64, error) { return s.snap.FreeBytes, nil }
func (s staticSnapshot) Snapshot() (capacity.DeviceSnapshot, error) {
	return s.snap, nil
}

func TestUnit_ServiceDescribeResolvesCapacity(t *testing.T) {
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticMemory(2<<20)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{MaxResidentBytes: 1 << 20, HeadroomFrac: 0.1}),
	)

	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 4096, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.ModelMaxContext != 32768 {
		t.Fatalf("ModelMaxContext = %d, want 32768", info.ModelMaxContext)
	}
	if info.EffectiveContext <= 0 || info.EffectiveContext >= 4096 {
		t.Fatalf("EffectiveContext = %d, want clamp below request", info.EffectiveContext)
	}
	if info.HotContextTokens != info.EffectiveContext || info.PlannerEffectiveContext != info.EffectiveContext {
		t.Fatalf("context split = hot %d planner %d effective %d", info.HotContextTokens, info.PlannerEffectiveContext, info.EffectiveContext)
	}
	if info.MemoryContextTokens < info.EffectiveContext {
		t.Fatalf("MemoryContextTokens = %d, want >= effective %d", info.MemoryContextTokens, info.EffectiveContext)
	}
	if !info.Clamped || info.UserLimitBytes != 1<<20 {
		t.Fatalf("capacity explanation missing clamp/user limit: %+v", info)
	}
}

func TestUnit_ServiceDescribeDefaultsResidentCapFromDetectedFreeMemory(t *testing.T) {
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticMemory(10<<20)),
		WithHostMemorySource(staticMemory(16<<20)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 1024, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.UserLimitBytes != 8<<20 {
		t.Fatalf("UserLimitBytes = %d, want 80%% of detected launch free", info.UserLimitBytes)
	}
	if info.HostColdBudgetBytes != 4<<20 {
		t.Fatalf("HostColdBudgetBytes = %d, want 25%% of host free", info.HostColdBudgetBytes)
	}
}

func TestUnit_ServiceDescribeReportsPlannerAboveHotWithHostColdBudget(t *testing.T) {
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticMemory(2<<20)),
		WithHostMemorySource(staticMemory(16<<20)),
		WithCapacityPolicy(capacity.Policy{MaxResidentBytes: 1 << 20, HeadroomFrac: 0.1}),
	)

	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.HotContextTokens != info.EffectiveContext {
		t.Fatalf("hot/effective = %d/%d, want equal dense hot window", info.HotContextTokens, info.EffectiveContext)
	}
	if info.PlannerEffectiveContext <= info.HotContextTokens {
		t.Fatalf("PlannerEffectiveContext = %d, want above hot %d", info.PlannerEffectiveContext, info.HotContextTokens)
	}
	if info.HostColdBudgetBytes != 4<<20 {
		t.Fatalf("HostColdBudgetBytes = %d, want default 4MiB", info.HostColdBudgetBytes)
	}
}

func TestUnit_ServiceDescribeReportsSparseAttentionForSWA(t *testing.T) {
	path := writeTestGGUFWithSWA(t, 32768, 4096)
	svc := NewService(
		WithMemorySource(staticMemory(64<<20)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 4096, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !info.SparseAttention || info.SlidingWindowAttentionTokens != 4096 {
		t.Fatalf("sparse attention metadata = enabled %v window %d, want true/4096", info.SparseAttention, info.SlidingWindowAttentionTokens)
	}
}

func TestUnit_ServiceOpenSessionRejectsOversizedContextBeforeBackend(t *testing.T) {
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticMemory(2<<20)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{MaxResidentBytes: 1 << 20, HeadroomFrac: 0.1}),
	)

	_, err := svc.OpenSession(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 4096, KVCacheType: "f16"},
	})
	if !errors.Is(err, transport.ErrContextOverflow) {
		t.Fatalf("OpenSession = %v, want ErrContextOverflow", err)
	}
}

// TestUnit_ServiceRefusesAutoSessionBelowUsableContextFloor is the regression
// guard for the sub-usable-window bug: under memory pressure the resolver used to
// stop shedding the instant context went positive and open sessions of a few
// hundred tokens — a window too small to hold a system prompt, so the model
// silently degraded (wrong language, ignored instructions). With the default
// floor, an auto session whose best achievable window is below the floor must be
// refused loudly, and a session with headroom must clear the floor and resolve.
func TestUnit_ServiceRefusesAutoSessionBelowUsableContextFloor(t *testing.T) {
	path := writeTestGGUF(t, 32768) // model max well above the floor

	// Tight budget: leaves room for a positive but sub-floor KV window.
	tight := NewService(
		WithMemorySource(staticMemory(3<<20)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)
	if _, err := tight.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"}, // NumCtx=0 => auto, modeld owns the window
	}); !errors.Is(err, transport.ErrContextOverflow) {
		t.Fatalf("tight auto resolveConfig = %v, want ErrContextOverflow (refuse below usable floor)", err)
	}

	// Comfortable budget: the auto window clears the floor and resolves.
	roomy := NewService(
		WithMemorySource(staticMemory(256<<20)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)
	cfg, err := roomy.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("roomy auto resolveConfig: %v", err)
	}
	if cfg.NumCtx < DefaultMinHotContextTokens {
		t.Fatalf("roomy auto NumCtx = %d, want >= usable floor %d", cfg.NumCtx, DefaultMinHotContextTokens)
	}
}

func TestUnit_ServiceMinHotContextCanBeDisabled(t *testing.T) {
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticMemory(3<<20)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{MinHotContextTokens: -1, HeadroomFrac: 0.1}),
	)
	cfg, err := svc.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("resolveConfig with floor disabled: %v", err)
	}
	if cfg.NumCtx <= 0 || cfg.NumCtx >= DefaultMinHotContextTokens {
		t.Fatalf("NumCtx with floor disabled = %d, want positive sub-default window", cfg.NumCtx)
	}
}

func TestUnit_ServiceRefusesSubFloorAcceleratorFitInsteadOfSilentCPUFallback(t *testing.T) {
	t.Setenv("CONTENOX_LLAMA_GPU_COMPUTE_RESERVE", "1MiB")
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticSnapshot{snap: capacity.DeviceSnapshot{
			Kind:       "gpu",
			DeviceID:   "test-gpu",
			TotalBytes: 4 << 20,
			FreeBytes:  4 << 20,
		}}),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.ResolvedGpuLayers <= 0 {
		t.Fatalf("ResolvedGpuLayers = %d, want best positive accelerator fit", info.ResolvedGpuLayers)
	}
	if info.HotContextTokens >= DefaultMinHotContextTokens {
		t.Fatalf("HotContextTokens = %d, want sub-floor fit", info.HotContextTokens)
	}
	if _, err := svc.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	}); !errors.Is(err, transport.ErrContextOverflow) {
		t.Fatalf("resolveConfig = %v, want ErrContextOverflow instead of silent CPU fallback", err)
	}
}

// TestUnit_ServiceAutoFullOffloadWithRoomTakesAllLayers is the happy no-spill
// case: an accelerator with ample memory, auto mode (no explicit cap, no num_ctx).
// modeld offloads every layer and serves a real auto context — it does NOT walk
// layers down, so ResolvedGpuLayers is the full model depth (blocks + output).
func TestUnit_ServiceAutoFullOffloadWithRoomTakesAllLayers(t *testing.T) {
	params := ggufParams{ContextLength: 32768, BlockCount: 2, HeadCountKV: 1, HeadCount: 2, KeyLength: 128}
	path := writeTestModelProfile(t, params)
	svc := NewService(
		WithMemorySource(staticSnapshot{snap: capacity.DeviceSnapshot{
			Kind:       "gpu",
			DeviceID:   "test-gpu",
			TotalBytes: 16 << 30,
			FreeBytes:  16 << 30,
		}}),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.ResolvedGpuLayers != params.BlockCount+1 {
		t.Fatalf("ResolvedGpuLayers = %d, want full offload %d (all layers, no walk-down)", info.ResolvedGpuLayers, params.BlockCount+1)
	}
	if info.EffectiveContext <= 0 {
		t.Fatalf("EffectiveContext = %d, want a positive auto window at full offload", info.EffectiveContext)
	}
	cfg, err := svc.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.NumGpuLayers != params.BlockCount+1 {
		t.Fatalf("cfg.NumGpuLayers = %d, want full offload %d", cfg.NumGpuLayers, params.BlockCount+1)
	}
	if cfg.NumCtx < DefaultMinHotContextTokens {
		t.Fatalf("auto NumCtx = %d, want a sensible window >= floor %d", cfg.NumCtx, DefaultMinHotContextTokens)
	}
}

// TestUnit_ServiceAutoRefusesModelTooBigForFullOffloadInsteadOfPartialSpill is the
// core no-spill guard: a model whose weights do not fit even at full offload must
// be REFUSED with the budget arithmetic — not partial-offloaded (weights + KV split
// onto the CPU). It asserts the placement modeld reports is full offload (all
// layers), i.e. it never fell back to a partial-CPU spill to make the model "fit".
func TestUnit_ServiceAutoRefusesModelTooBigForFullOffloadInsteadOfPartialSpill(t *testing.T) {
	t.Setenv("CONTENOX_LLAMA_GPU_COMPUTE_RESERVE", "1MiB")
	params := ggufParams{ContextLength: 32768, BlockCount: 2, HeadCountKV: 1, HeadCount: 2, KeyLength: 128}
	path := writeTestModelProfile(t, params)
	if err := os.Truncate(path, 200<<20); err != nil { // weights far exceed the device budget
		t.Fatalf("pad GGUF: %v", err)
	}
	svc := NewService(
		WithMemorySource(staticSnapshot{snap: capacity.DeviceSnapshot{
			Kind:       "gpu",
			DeviceID:   "test-gpu",
			TotalBytes: 256 << 20,
			FreeBytes:  100 << 20,
		}}),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.ResolvedGpuLayers != params.BlockCount+1 {
		t.Fatalf("ResolvedGpuLayers = %d, want full offload %d (no partial-CPU spill)", info.ResolvedGpuLayers, params.BlockCount+1)
	}
	if info.EffectiveContext > 0 {
		t.Fatalf("EffectiveContext = %d, want ~0 (does not fit) so the caller refuses", info.EffectiveContext)
	}
	_, err = svc.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	})
	if !errors.Is(err, transport.ErrContextOverflow) {
		t.Fatalf("resolveConfig = %v, want ErrContextOverflow (refuse, not spill)", err)
	}
	for _, want := range []string{"needs", "weights", "usable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q missing arithmetic term %q", err.Error(), want)
		}
	}
}

func TestUnit_ServiceResolveConfigAppliesDaemonEnvOverrides(t *testing.T) {
	t.Setenv("CONTENOX_LLAMA_GPU_LAYERS", "999")
	t.Setenv("CONTENOX_LLAMA_CTX", "16384")
	t.Setenv("CONTENOX_LLAMA_KV_CACHE_TYPE", "q8_0")
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		// An accelerator snapshot with ample memory: the daemon GPU-layer override
		// only materializes as offload when an accelerator is present (otherwise it
		// resolves to 0 — see TestUnit_ServiceZeroesDaemonGpuLayersWithoutAccelerator),
		// and ample memory keeps the budget from clamping it back to zero.
		WithMemorySource(staticSnapshot{snap: capacity.DeviceSnapshot{
			Kind:       "gpu",
			DeviceID:   "test-gpu",
			TotalBytes: 16 << 30,
			FreeBytes:  16 << 30,
		}}),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	cfg, err := svc.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 8192, NumGpuLayers: 0, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.NumGpuLayers <= 0 {
		t.Fatalf("NumGpuLayers = %d, want positive layer count from daemon env override 999", cfg.NumGpuLayers)
	}
	if cfg.NumCtx != 16384 {
		t.Fatalf("NumCtx = %d, want daemon env override 16384", cfg.NumCtx)
	}
	if cfg.KVCacheType != "q8_0" {
		t.Fatalf("KVCacheType = %q, want daemon env override q8_0", cfg.KVCacheType)
	}
}

func TestUnit_ServiceResolveConfigHonorsRequestedPlannerContext(t *testing.T) {
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticMemory(16<<20)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	cfg, err := svc.resolveConfig(transport.OpenSessionRequest{
		Type: "llama",
		Path: path,
		Config: transport.Config{
			NumCtx:                  256,
			PlannerEffectiveContext: 384,
			KVCacheType:             "f16",
		},
	})
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.NumCtx != 256 || cfg.HotContextTokens != 256 {
		t.Fatalf("hot config = num_ctx %d hot %d, want 256/256", cfg.NumCtx, cfg.HotContextTokens)
	}
	if cfg.PlannerEffectiveContext != 384 {
		t.Fatalf("PlannerEffectiveContext = %d, want requested logical planner 384", cfg.PlannerEffectiveContext)
	}
}

func TestUnit_ServiceAutoOffloadsToDetectedAccelerator(t *testing.T) {
	// No CONTENOX_LLAMA_GPU_LAYERS and no profile request (NumGpuLayers: 0): modeld
	// must still offload because it detected an accelerator with ample memory —
	// the value is derived from the device, not a knob.
	t.Setenv("CONTENOX_LLAMA_GPU_LAYERS", "")
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticSnapshot{snap: capacity.DeviceSnapshot{
			Kind:       "gpu",
			DeviceID:   "test-gpu",
			TotalBytes: 16 << 30,
			FreeBytes:  16 << 30,
		}}),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	cfg, err := svc.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 8192, NumGpuLayers: 0, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.NumGpuLayers <= 0 {
		t.Fatalf("NumGpuLayers = %d, want auto-offload (>0) on a detected accelerator without any request", cfg.NumGpuLayers)
	}
}

func TestUnit_ServiceZeroesDaemonGpuLayersWithoutAccelerator(t *testing.T) {
	t.Setenv("CONTENOX_LLAMA_GPU_LAYERS", "999")
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		// staticMemory yields a non-accelerator (system RAM) snapshot — the
		// universal-binary-on-CPU case.
		WithMemorySource(staticMemory(16<<30)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	cfg, err := svc.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 8192, NumGpuLayers: 0, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.NumGpuLayers != 0 {
		t.Fatalf("NumGpuLayers = %d, want 0 without an accelerator", cfg.NumGpuLayers)
	}

	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 8192, NumGpuLayers: 999, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.RequestedGpuLayers != 999 || info.ResolvedGpuLayers != 0 {
		t.Fatalf("gpu layer explanation = requested %d resolved %d, want 999/0", info.RequestedGpuLayers, info.ResolvedGpuLayers)
	}
	if !info.Clamped || info.Reason != "no_accelerator_present" {
		t.Fatalf("expected no_accelerator_present clamp explanation: %+v", info)
	}
}

func TestUnit_ServiceResolveConfigClampsDaemonGpuLayersToMemoryBudget(t *testing.T) {
	t.Setenv("CONTENOX_LLAMA_GPU_LAYERS", "999")
	t.Setenv("CONTENOX_LLAMA_GPU_COMPUTE_RESERVE", "8MiB")
	path := writeTestGGUF(t, 32768)
	if err := os.Truncate(path, 90<<20); err != nil {
		t.Fatalf("pad GGUF: %v", err)
	}
	svc := NewService(
		WithMemorySource(staticSnapshot{snap: capacity.DeviceSnapshot{
			Kind:       "gpu",
			DeviceID:   "test-gpu",
			TotalBytes: 256 << 20,
			FreeBytes:  130 << 20,
		}}),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	cfg, err := svc.resolveConfig(transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 8192, NumGpuLayers: 0, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.NumGpuLayers <= 0 || cfg.NumGpuLayers >= 999 {
		t.Fatalf("NumGpuLayers = %d, want clamped positive layer count", cfg.NumGpuLayers)
	}
	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 8192, NumGpuLayers: 999, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.RequestedGpuLayers != 999 || info.ResolvedGpuLayers != cfg.NumGpuLayers {
		t.Fatalf("gpu layer explanation = requested %d resolved %d, cfg %d", info.RequestedGpuLayers, info.ResolvedGpuLayers, cfg.NumGpuLayers)
	}
	if !info.Clamped || info.Reason == "" {
		t.Fatalf("expected clamped gpu-layer explanation: %+v", info)
	}
}

// TestUnit_ResolveGPULayersForBudgetShedsForMinHotContextUnderExplicitCap is the
// regression guard that the operator's explicit GPU-layer cap still re-enables the
// walk-down: with an explicit cap of 5 and an auto (unpinned) context window, the
// resolver sheds layers to reach the min-hot floor exactly as before. Shedding is a
// deliberate operator choice, not the auto default.
func TestUnit_ResolveGPULayersForBudgetShedsForMinHotContextUnderExplicitCap(t *testing.T) {
	params := ggufParams{ContextLength: 10000, BlockCount: 4}
	st := capacity.DeviceSnapshot{Kind: "gpu", FreeBytes: 1000}
	policy := capacity.Policy{MinHotContextTokens: 600, HeadroomFrac: 0.000001}

	got := resolveGPULayersForBudget(
		5, // explicit operator cap → walk-down reachable
		transport.Config{NumGpuLayers: 5},
		params,
		capacity.LayerKVProfile{},
		500,
		1,
		0,
		st,
		policy,
	)
	if got != 3 {
		t.Fatalf("gpu layers = %d, want 3: 5/4 slots miss the 600-token hot target, 3 slots reaches it", got)
	}
}

// TestUnit_ResolveGPULayersForBudgetNeverShedsInAutoMode proves the no-spill rule:
// with neither an explicit GPU-layer cap (explicitGpuLayers <= 0) nor an explicit
// num_ctx, modeld stays at full offload even though shedding to 3 layers would buy
// the 600-token min-hot window. Same tight budget as the explicit-cap case above;
// only the auto-vs-explicit distinction changes the outcome.
func TestUnit_ResolveGPULayersForBudgetNeverShedsInAutoMode(t *testing.T) {
	params := ggufParams{ContextLength: 10000, BlockCount: 4}
	st := capacity.DeviceSnapshot{Kind: "gpu", FreeBytes: 1000}
	policy := capacity.Policy{MinHotContextTokens: 600, HeadroomFrac: 0.000001}

	got := resolveGPULayersForBudget(
		0, // auto: no explicit cap
		transport.Config{NumGpuLayers: allGpuLayers}, // ceiling modeld aims for in auto mode
		params,
		capacity.LayerKVProfile{},
		500,
		1,
		0,
		st,
		policy,
	)
	if got != params.BlockCount+1 {
		t.Fatalf("gpu layers = %d, want full offload %d (no walk-down in auto mode)", got, params.BlockCount+1)
	}
}

func TestUnit_ResolveGPULayersForBudgetIgnoresMinHotContextForExplicitNumCtx(t *testing.T) {
	params := ggufParams{ContextLength: 10000, BlockCount: 4}
	st := capacity.DeviceSnapshot{Kind: "gpu", FreeBytes: 1000}
	policy := capacity.Policy{MinHotContextTokens: 600, HeadroomFrac: 0.000001}

	got := resolveGPULayersForBudget(
		5,
		transport.Config{NumCtx: 400, NumGpuLayers: 5},
		params,
		capacity.LayerKVProfile{},
		500,
		1,
		0,
		st,
		policy,
	)
	if got != 5 {
		t.Fatalf("gpu layers = %d, want explicit num_ctx to preserve maximum fitting offload", got)
	}
}

func writeTestGGUF(t *testing.T, ctx int) string {
	return writeTestGGUFWithSWA(t, ctx, 0)
}

func writeTestGGUFWithSWA(t *testing.T, ctx, slidingWindow int) string {
	t.Helper()
	return writeTestModelProfile(t, ggufParams{
		ContextLength: ctx,
		BlockCount:    2,
		HeadCountKV:   1,
		HeadCount:     2,
		KeyLength:     128,
		SlidingWindow: slidingWindow,
	})
}

func writeTestModelProfile(t *testing.T, params ggufParams) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, []byte("test model bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := inspectLlamaModel
	inspectLlamaModel = func(got string) (ggufParams, error) {
		if got != path {
			t.Fatalf("inspectLlamaModel path = %q, want %q", got, path)
		}
		return params, nil
	}
	t.Cleanup(func() { inspectLlamaModel = old })
	return path
}

func TestUnit_ServiceDescribeAppliesReclaimableBytesToFreeMemory(t *testing.T) {
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticMemory(4<<20)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)

	base, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	credited, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:             "llama",
		Path:             path,
		Config:           transport.Config{KVCacheType: "f16"},
		ReclaimableBytes: 4 << 20,
	})
	if err != nil {
		t.Fatalf("Describe with credit: %v", err)
	}
	if credited.FreeBytes != base.FreeBytes+4<<20 {
		t.Fatalf("FreeBytes = %d, want snapshot %d plus reclaim credit", credited.FreeBytes, base.FreeBytes)
	}
	if credited.EffectiveContext <= base.EffectiveContext {
		t.Fatalf("EffectiveContext = %d, want above uncredited %d", credited.EffectiveContext, base.EffectiveContext)
	}
	// The credit must land before policy derivation so the live 80%%-of-free
	// resident cap sees the post-switch picture too.
	if credited.UserLimitBytes <= base.UserLimitBytes {
		t.Fatalf("UserLimitBytes = %d, want above uncredited %d", credited.UserLimitBytes, base.UserLimitBytes)
	}
}
