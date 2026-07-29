package capacity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnit_KVBytesPerToken(t *testing.T) {
	// 28 layers, 2 KV heads, 128 head dim, f16 (2 bytes), K+V:
	// 2 * 28 * 2 * 128 * 2 = 28672 bytes/token.
	if got := KVBytesPerToken(28, 2, 128, "f16"); got != 28672 {
		t.Fatalf("KVBytesPerToken = %d, want 28672", got)
	}
	if got := KVBytesPerToken(28, 2, 128, "q8_0"); got != 14336 {
		t.Fatalf("q8_0 KVBytesPerToken = %d, want 14336", got)
	}
	if got := KVBytesPerToken(0, 2, 128, "f16"); got != 0 {
		t.Fatalf("unknown layers should yield 0, got %d", got)
	}
}

func TestUnit_LayerKVProfile_SWABytesForContext(t *testing.T) {
	densePerToken := int64(168 << 10)
	p := LayerKVProfile{
		GlobalLayers:    7,
		WindowedLayers:  35,
		Window:          512,
		PerLayerKVBytes: densePerToken / 42,
	}
	got := p.KVBytesForContext(131072)
	want := int64(7)*p.PerLayerKVBytes*131072 + int64(35)*p.PerLayerKVBytes*512
	if got != want {
		t.Fatalf("KVBytesForContext = %d, want %d", got, want)
	}
	const gib = int64(1 << 30)
	if got < 3*gib || got > 4*gib {
		t.Fatalf("SWA context cost = %.2f GiB, want about 3.6 GiB", float64(got)/float64(gib))
	}
	dense := densePerToken * 131072
	if dense/got < 5 {
		t.Fatalf("SWA cost %d should be far below dense %d", got, dense)
	}
}

func TestUnit_Resolve_SWAProfileInvertsBudget(t *testing.T) {
	densePerToken := int64(168 << 10)
	profile := LayerKVProfile{
		GlobalLayers:    7,
		WindowedLayers:  35,
		Window:          512,
		PerLayerKVBytes: densePerToken / 42,
	}
	need := profile.KVBytesForContext(131072)
	c := Resolve(Params{
		ModelMaxCtx:     131072,
		KVBytesPerToken: densePerToken,
		LayerKV:         profile,
		FreeBytes:       need,
		HeadroomFrac:    0.000001,
	})
	if c.EffectiveContext < 131000 {
		t.Fatalf("EffectiveContext = %d, want near full 131072 SWA window", c.EffectiveContext)
	}
	denseOnly := Resolve(Params{
		ModelMaxCtx:     131072,
		KVBytesPerToken: densePerToken,
		FreeBytes:       need,
		HeadroomFrac:    0.000001,
	})
	if denseOnly.EffectiveContext >= c.EffectiveContext/2 {
		t.Fatalf("dense-only context = %d, SWA context = %d; dense path should remain much smaller", denseOnly.EffectiveContext, c.EffectiveContext)
	}
}

func TestUnit_Resolve_DenseLayerKVProfileMatchesFlatPath(t *testing.T) {
	flat := Resolve(Params{
		ModelMaxCtx:     32768,
		KVBytesPerToken: 28672,
		WeightsBytes:    4 << 20,
		OverheadBytes:   2 << 20,
		FreeBytes:       10 << 20,
		HeadroomFrac:    0.1,
	})
	profiled := Resolve(Params{
		ModelMaxCtx:     32768,
		KVBytesPerToken: 28672,
		LayerKV: LayerKVProfile{
			GlobalLayers:    28,
			PerLayerKVBytes: 28672 / 28,
		},
		WeightsBytes:  4 << 20,
		OverheadBytes: 2 << 20,
		FreeBytes:     10 << 20,
		HeadroomFrac:  0.1,
	})
	if profiled.EffectiveContext != flat.EffectiveContext ||
		profiled.MemoryContextTokens != flat.MemoryContextTokens ||
		profiled.HotContextTokens != flat.HotContextTokens ||
		profiled.PlannerEffectiveContext != flat.PlannerEffectiveContext ||
		profiled.RequiredBytes != flat.RequiredBytes {
		t.Fatalf("profiled dense resolve = %+v, want flat %+v", profiled, flat)
	}
}

func TestUnit_Resolve_AmpleMemoryUsesModelMax(t *testing.T) {
	c := Resolve(Params{
		ModelMaxCtx:     32768,
		KVBytesPerToken: 28672,
		WeightsBytes:    1 << 30,  // 1 GiB weights
		FreeBytes:       64 << 30, // 64 GiB free
		HeadroomFrac:    0.1,
	})
	if c.EffectiveContext != 32768 {
		t.Fatalf("ample memory should give model max 32768, got %d", c.EffectiveContext)
	}
}

func TestUnit_Resolve_ScarceMemoryClampsBelowMax(t *testing.T) {
	// ~512 MiB free, 28672 B/token → budget ≈ 0.9*512MiB ≈ 483MiB → ~17.6k tokens.
	c := Resolve(Params{
		ModelMaxCtx:     32768,
		KVBytesPerToken: 28672,
		WeightsBytes:    0,
		FreeBytes:       512 << 20,
		HeadroomFrac:    0.1,
	})
	if c.EffectiveContext <= 0 || c.EffectiveContext >= 32768 {
		t.Fatalf("scarce memory should clamp below model max, got %d", c.EffectiveContext)
	}
	free := int64(512 << 20)
	want := int(int64(float64(free)*0.9) / 28672)
	if c.EffectiveContext != want {
		t.Fatalf("EffectiveContext = %d, want %d", c.EffectiveContext, want)
	}
	if c.MemoryContextTokens != want || c.HotContextTokens != want || c.PlannerEffectiveContext != want {
		t.Fatalf("context split = memory %d hot %d planner %d, want all %d",
			c.MemoryContextTokens, c.HotContextTokens, c.PlannerEffectiveContext, want)
	}
}

func TestUnit_Resolve_RequestCaps(t *testing.T) {
	c := Resolve(Params{ModelMaxCtx: 32768, KVBytesPerToken: 28672, FreeBytes: 64 << 30, Request: 8192})
	if c.EffectiveContext != 8192 {
		t.Fatalf("request should cap to 8192, got %d", c.EffectiveContext)
	}
	if c.MemoryContextTokens <= c.EffectiveContext {
		t.Fatalf("MemoryContextTokens = %d, want raw memory-fit budget above requested effective %d", c.MemoryContextTokens, c.EffectiveContext)
	}
	if c.HotContextTokens != c.EffectiveContext || c.PlannerEffectiveContext != c.EffectiveContext {
		t.Fatalf("hot/planner = %d/%d, want effective %d", c.HotContextTokens, c.PlannerEffectiveContext, c.EffectiveContext)
	}
}

func TestUnit_Resolve_HostColdBudgetExpandsPlannerContext(t *testing.T) {
	c := Resolve(Params{
		ModelMaxCtx:         8192,
		KVBytesPerToken:     1024,
		FreeBytes:           2 << 20,
		HostColdBudgetBytes: 3 << 20,
		HeadroomFrac:        0.5,
	})
	if c.EffectiveContext != 1024 || c.HotContextTokens != 1024 {
		t.Fatalf("dense/hot = %d/%d, want 1024/1024", c.EffectiveContext, c.HotContextTokens)
	}
	if c.PlannerEffectiveContext != 4096 {
		t.Fatalf("PlannerEffectiveContext = %d, want hot 1024 + cold 3072", c.PlannerEffectiveContext)
	}
	if c.HostColdBudgetBytes != 3<<20 {
		t.Fatalf("HostColdBudgetBytes = %d, want carried policy", c.HostColdBudgetBytes)
	}
}

func TestUnit_Resolve_RequestCapsPlannerContext(t *testing.T) {
	c := Resolve(Params{
		ModelMaxCtx:         8192,
		KVBytesPerToken:     1024,
		FreeBytes:           2 << 20,
		HostColdBudgetBytes: 3 << 20,
		Request:             2048,
		HeadroomFrac:        0.5,
	})
	if c.EffectiveContext != 1024 || c.HotContextTokens != 1024 {
		t.Fatalf("dense/hot = %d/%d, want 1024/1024", c.EffectiveContext, c.HotContextTokens)
	}
	if c.PlannerEffectiveContext != 2048 {
		t.Fatalf("PlannerEffectiveContext = %d, want request cap 2048", c.PlannerEffectiveContext)
	}
}

func TestUnit_Resolve_UnknownKVFallsBackToModelMax(t *testing.T) {
	c := Resolve(Params{ModelMaxCtx: 32768, KVBytesPerToken: 0, FreeBytes: 1 << 20})
	if c.EffectiveContext != 32768 {
		t.Fatalf("unknown KV cost should fall back to model max, got %d", c.EffectiveContext)
	}
	if c.MemoryContextTokens != 0 || c.HotContextTokens != 32768 || c.PlannerEffectiveContext != 32768 {
		t.Fatalf("unknown-KV context split = memory %d hot %d planner %d, want 0/32768/32768",
			c.MemoryContextTokens, c.HotContextTokens, c.PlannerEffectiveContext)
	}
}

func TestUnit_Resolve_WeightsExceedMemoryYieldsZero(t *testing.T) {
	c := Resolve(Params{ModelMaxCtx: 32768, KVBytesPerToken: 28672, WeightsBytes: 2 << 30, FreeBytes: 1 << 30})
	if c.EffectiveContext != 0 {
		t.Fatalf("weights exceeding memory should yield 0 window, got %d", c.EffectiveContext)
	}
}

func TestUnit_Resolve_RequestDoesNotReviveImpossibleMemoryBudget(t *testing.T) {
	c := Resolve(Params{
		ModelMaxCtx:     32768,
		KVBytesPerToken: 1024,
		WeightsBytes:    10 << 20,
		OverheadBytes:   1 << 20,
		FreeBytes:       8 << 20,
		Request:         4096,
		HeadroomFrac:    0.1,
	})
	if c.EffectiveContext != 0 {
		t.Fatalf("impossible memory budget with request should stay 0, got %d", c.EffectiveContext)
	}
}

func TestUnit_Resolve_UserLimitAndReserveClamp(t *testing.T) {
	c := Resolve(Params{
		ModelMaxCtx:     32768,
		KVBytesPerToken: 1024,
		WeightsBytes:    1 << 20,
		FreeBytes:       64 << 20,
		UserLimitBytes:  8 << 20,
		MinFreeBytes:    4 << 20,
		HeadroomFrac:    0.1,
	})
	if c.EffectiveContext >= 32768 {
		t.Fatalf("user limit should clamp below model max, got %d", c.EffectiveContext)
	}
	if !c.Clamped {
		t.Fatal("expected clamped capacity")
	}
	if c.UserLimitBytes != 8<<20 || c.MinFreeBytes != 4<<20 {
		t.Fatalf("policy fields not carried through: %+v", c)
	}
}

func TestUnit_Resolve_OverheadConsumesBudget(t *testing.T) {
	c := Resolve(Params{
		ModelMaxCtx:     32768,
		KVBytesPerToken: 1024,
		WeightsBytes:    4 << 20,
		OverheadBytes:   2 << 20,
		FreeBytes:       10 << 20,
		HeadroomFrac:    0.1,
	})
	want := int(((9 << 20) - (4 << 20) - (2 << 20)) / 1024)
	if c.EffectiveContext != want {
		t.Fatalf("EffectiveContext = %d, want %d", c.EffectiveContext, want)
	}
	if c.RequiredBytes != c.WeightsBytes+c.OverheadBytes+int64(c.EffectiveContext)*c.KVBytesPerToken {
		t.Fatalf("RequiredBytes does not include overhead: %+v", c)
	}
}

func TestUnit_WithResidentDefault_SetsMissingResidentCap(t *testing.T) {
	p := WithResidentDefault(Policy{}, DeviceSnapshot{FreeBytes: 10 << 20})
	if p.MaxResidentBytes != 8<<20 {
		t.Fatalf("MaxResidentBytes = %d, want 80%% of current free", p.MaxResidentBytes)
	}

	explicit := WithResidentDefault(Policy{MaxResidentBytes: 3 << 20}, DeviceSnapshot{FreeBytes: 10 << 20})
	if explicit.MaxResidentBytes != 3<<20 {
		t.Fatalf("explicit max should win, got %d", explicit.MaxResidentBytes)
	}
}

func TestUnit_WithHostColdDefaults_SetsMissingColdBudget(t *testing.T) {
	p := WithHostColdDefaults(Policy{}, DeviceSnapshot{FreeBytes: 16 << 20})
	if p.HostColdBudgetBytes != 4<<20 {
		t.Fatalf("HostColdBudgetBytes = %d, want 25%% of host free", p.HostColdBudgetBytes)
	}

	explicit := WithHostColdDefaults(Policy{HostColdBudgetBytes: 3 << 20}, DeviceSnapshot{FreeBytes: 16 << 20})
	if explicit.HostColdBudgetBytes != 3<<20 {
		t.Fatalf("explicit host cold should win, got %d", explicit.HostColdBudgetBytes)
	}
}

func TestUnit_WithResidentDefault_TracksCurrentFree(t *testing.T) {
	dev := DeviceSnapshot{Kind: "gpu", DeviceID: "0", TotalBytes: 16 << 20}

	// Tight launch window — e.g. another process (or a stale modeld mid-teardown)
	// still holds VRAM, so the device reports little free memory.
	dev.FreeBytes = 4 << 20
	tight := WithResidentDefault(Policy{}, dev)
	if want := int64(float64(dev.FreeBytes) * DefaultMaxResidentFrac); tight.MaxResidentBytes != want {
		t.Fatalf("tight MaxResidentBytes = %d, want %d", tight.MaxResidentBytes, want)
	}

	// Same device, memory later freed up: the default cap must RISE to match the
	// current free memory, not stay frozen at the tight launch value. This is the
	// regression that pinned offload to ~0 layers after a stale process was killed.
	dev.FreeBytes = 12 << 20
	roomy := WithResidentDefault(Policy{}, dev)
	if want := int64(float64(dev.FreeBytes) * DefaultMaxResidentFrac); roomy.MaxResidentBytes != want {
		t.Fatalf("roomy MaxResidentBytes = %d, want %d (cap must track current free, not stick)", roomy.MaxResidentBytes, want)
	}

	// An explicit user cap is always honored and never overwritten by the default.
	explicit := WithResidentDefault(Policy{MaxResidentBytes: 3 << 20}, dev)
	if explicit.MaxResidentBytes != 3<<20 {
		t.Fatalf("explicit MaxResidentBytes = %d, want 3MiB", explicit.MaxResidentBytes)
	}
}

func TestUnit_ParseBytes(t *testing.T) {
	cases := map[string]int64{
		"1024":   1024,
		"1KiB":   1 << 10,
		"1.5GiB": int64(1.5 * float64(1<<30)),
		"2GB":    2_000_000_000,
	}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil {
			t.Fatalf("ParseBytes(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestUnit_LoadPolicy_FromConfigAndEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "modeld.json"), []byte(`{"memory":{"max_resident":"4GiB","reserve_free":"1GiB","host_cold_budget":"8GiB","min_hot_context_tokens":4096,"headroom_frac":0.2}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTENOX_MODELD_MEM_MAX", "2GiB")
	t.Setenv("CONTENOX_MODELD_MEM_RESERVE", "")
	t.Setenv("CONTENOX_MODELD_MEM_COLD", "6GiB")
	t.Setenv("CONTENOX_MODELD_MEM_HEADROOM", "")
	t.Setenv("CONTENOX_MODELD_MIN_HOT_CONTEXT", "8192")
	p := LoadPolicy(dir)
	if p.MaxResidentBytes != 2<<30 {
		t.Fatalf("MaxResidentBytes = %d, want env override 2GiB", p.MaxResidentBytes)
	}
	if p.MinFreeBytes != 1<<30 {
		t.Fatalf("MinFreeBytes = %d, want config 1GiB", p.MinFreeBytes)
	}
	if p.HostColdBudgetBytes != 6<<30 {
		t.Fatalf("HostColdBudgetBytes = %d, want env override 6GiB", p.HostColdBudgetBytes)
	}
	if p.MinHotContextTokens != 8192 {
		t.Fatalf("MinHotContextTokens = %d, want env override 8192", p.MinHotContextTokens)
	}
	if p.HeadroomFrac != 0.2 {
		t.Fatalf("HeadroomFrac = %v, want config 0.2", p.HeadroomFrac)
	}
}

func TestUnit_LoadPolicy_MinHotContextZeroDisables(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "modeld.json"), []byte(`{"memory":{"min_hot_context_tokens":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := LoadPolicy(dir)
	if p.MinHotContextTokens >= 0 {
		t.Fatalf("config MinHotContextTokens = %d, want explicit-off sentinel", p.MinHotContextTokens)
	}

	t.Setenv("CONTENOX_MODELD_MIN_HOT_CONTEXT", "0")
	p = LoadPolicy(t.TempDir())
	if p.MinHotContextTokens >= 0 {
		t.Fatalf("env MinHotContextTokens = %d, want explicit-off sentinel", p.MinHotContextTokens)
	}
}

func TestUnit_SystemRAM_ReportsPositive(t *testing.T) {
	got, err := SystemRAM{}.FreeBytes()
	if err != nil {
		t.Fatalf("SystemRAM: %v", err)
	}
	if got <= 0 {
		t.Fatalf("SystemRAM free = %d, want > 0", got)
	}
}

func TestUnit_WithResidentDefault_AcceleratorGetsMinFreeFloor(t *testing.T) {
	// 6 GiB card: 10% (614MiB) beats the 512MiB flat floor.
	p := WithResidentDefault(Policy{}, DeviceSnapshot{Kind: "gpu", TotalBytes: 6 << 30, FreeBytes: 5 << 30})
	total := int64(6 << 30)
	want := int64(float64(total) * DefaultAcceleratorMinFreeFrac)
	if p.MinFreeBytes != want {
		t.Fatalf("MinFreeBytes = %d, want %d (10%% of total)", p.MinFreeBytes, want)
	}

	// Tiny/unknown total: flat 512MiB floor wins.
	p = WithResidentDefault(Policy{}, DeviceSnapshot{Kind: "gpu", FreeBytes: 2 << 30})
	if p.MinFreeBytes != DefaultAcceleratorMinFreeBytes {
		t.Fatalf("MinFreeBytes = %d, want flat default %d", p.MinFreeBytes, DefaultAcceleratorMinFreeBytes)
	}

	// Explicit user reserve always wins.
	p = WithResidentDefault(Policy{MinFreeBytes: 1}, DeviceSnapshot{Kind: "gpu", TotalBytes: 6 << 30, FreeBytes: 5 << 30})
	if p.MinFreeBytes != 1 {
		t.Fatalf("MinFreeBytes = %d, want explicit value preserved", p.MinFreeBytes)
	}

	// Tiny device with known total: floor is capped at a quarter of the device
	// so the reserve cannot make the card unusable.
	p = WithResidentDefault(Policy{}, DeviceSnapshot{Kind: "gpu", TotalBytes: 256 << 20, FreeBytes: 130 << 20})
	if p.MinFreeBytes != 64<<20 {
		t.Fatalf("MinFreeBytes = %d, want 64MiB (25%% cap on tiny device)", p.MinFreeBytes)
	}

	// System RAM devices get no floor by default.
	p = WithResidentDefault(Policy{}, DeviceSnapshot{Kind: "system", TotalBytes: 16 << 30, FreeBytes: 8 << 30})
	if p.MinFreeBytes != 0 {
		t.Fatalf("MinFreeBytes = %d, want 0 for non-accelerator", p.MinFreeBytes)
	}
}
