// Package capacity is modeld's hardware capacity planner: it resolves the
// EFFECTIVE context window a model can actually be served at on this device,
// from the model's KV-cache footprint and the device's free memory — not the
// model's trained ceiling alone. modeld owns this calculation because it owns
// the backend process and hardware telemetry; the runtime consumes the resolved
// value and does not inspect model files.
package capacity

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/mem"
)

// DefaultHeadroomFrac of free memory is reserved for activations, the compute
// graph, and fragmentation, leaving the rest for model weights + KV cache.
const DefaultHeadroomFrac = 0.1

// DefaultMaxResidentFrac caps modeld's resident footprint at this fraction of
// the device's CURRENTLY free memory when the user did not set an explicit
// ceiling. It is evaluated fresh on every resolution, so the budget tracks the
// device live instead of freezing a launch-time view.
const DefaultMaxResidentFrac = 0.8

// DefaultHostColdFrac is the launch-time cap for the host-RAM KV cold store
// when the user did not set one explicitly.
const DefaultHostColdFrac = 0.25

// Accelerator devices are shared with every other process holding a context on
// them (compositors, browsers, editors via PRIME render offload — even when the
// display is wired to another GPU). Without a reserve floor, modeld's
// 80%-of-free budget plus weight load/free churn can starve those clients of
// VRAM. When the user set no explicit MinFreeBytes, accelerators default to
// max(DefaultAcceleratorMinFreeBytes, DefaultAcceleratorMinFreeFrac × total).
const (
	DefaultAcceleratorMinFreeBytes int64 = 512 << 20
	DefaultAcceleratorMinFreeFrac        = 0.10
)

// kvTypeBytes is the per-element size of one KV cache entry for a precision.
// KV is two tensors (K and V); KVBytesPerToken accounts for both. Quantized KV
// rounds up to a whole byte — KV is tiny next to weights, so over-estimating is
// the safe direction for a no-spill budget.
func kvTypeBytes(kvType string) int64 {
	switch kvType {
	case "", "f16", "fp16":
		return 2
	case "f32", "fp32":
		return 4
	case "q8_0":
		return 1
	case "q4_0", "q4_1":
		return 1
	default:
		return 2
	}
}

// KVBytesPerToken is the memory one token of context costs in the KV cache:
// K and V, across every layer and KV head, at the KV precision.
func KVBytesPerToken(nLayers, nKVHeads, headDim int, kvType string) int64 {
	if nLayers <= 0 || nKVHeads <= 0 || headDim <= 0 {
		return 0
	}
	return 2 * int64(nLayers) * int64(nKVHeads) * int64(headDim) * kvTypeBytes(kvType)
}

// LayerKVProfile describes how KV grows with context for one model.
// GlobalLayers grow linearly with context; WindowedLayers are capped at Window
// tokens. PerLayerKVBytes is the K+V cost for one token in one layer.
type LayerKVProfile struct {
	GlobalLayers    int
	WindowedLayers  int
	Window          int
	PerLayerKVBytes int64
}

// Valid reports whether the profile contains enough data to budget KV cache.
func (p LayerKVProfile) Valid() bool {
	return p.PerLayerKVBytes > 0 && p.totalLayers() > 0
}

// KVBytesForContext returns the total KV bytes needed to hold ctx tokens.
func (p LayerKVProfile) KVBytesForContext(ctx int) int64 {
	if !p.Valid() || ctx <= 0 {
		return 0
	}
	global, windowed := max(p.GlobalLayers, 0), max(p.WindowedLayers, 0)
	tokens := int64(ctx)
	if p.Window <= 0 {
		return int64(global+windowed) * p.PerLayerKVBytes * tokens
	}
	windowTokens := min(ctx, p.Window)
	return int64(global)*p.PerLayerKVBytes*tokens + int64(windowed)*p.PerLayerKVBytes*int64(windowTokens)
}

// MarginalKVBytesPerToken is the post-window growth cost of one more token.
func (p LayerKVProfile) MarginalKVBytesPerToken() int64 {
	if !p.Valid() {
		return 0
	}
	if p.Window <= 0 {
		return p.DenseKVBytesPerToken()
	}
	return int64(max(p.GlobalLayers, 0)) * p.PerLayerKVBytes
}

// DenseKVBytesPerToken is the legacy all-layers cost of one token.
func (p LayerKVProfile) DenseKVBytesPerToken() int64 {
	if !p.Valid() {
		return 0
	}
	return int64(p.totalLayers()) * p.PerLayerKVBytes
}

func (p LayerKVProfile) totalLayers() int {
	return max(p.GlobalLayers, 0) + max(p.WindowedLayers, 0)
}

func (p LayerKVProfile) tokensForBudget(budget int64, limit int) int {
	if !p.Valid() || budget <= 0 {
		return 0
	}
	if p.Window <= 0 || p.WindowedLayers <= 0 {
		if dense := p.DenseKVBytesPerToken(); dense > 0 {
			return int(budget / dense)
		}
		return 0
	}
	dense := p.DenseKVBytesPerToken()
	windowCost := dense * int64(p.Window)
	if budget <= windowCost {
		return int(budget / dense)
	}
	marginal := p.MarginalKVBytesPerToken()
	if marginal <= 0 {
		if limit > 0 {
			return limit
		}
		return p.Window
	}
	return int((budget - int64(max(p.WindowedLayers, 0))*p.PerLayerKVBytes*int64(p.Window)) / marginal)
}

// HardContextLimit is a genuinely-current explicit context ceiling: a value a
// user or operator set right now (CONTENOX_LLAMA_CTX, a model profile's
// runtime.num_ctx, or the equivalent OpenVINO knob). Resolve always honors a
// positive value as a hard ceiling, even when more memory is available.
//
// NEVER populate this from an EffectiveContext, HotContextTokens, or
// PlannerEffectiveContext that Resolve (or a prior Describe/OpenSession
// round-trip) itself returned earlier: feeding a stale advisory value back in
// as a request freezes every future resolution at it, even after the memory
// that constrained it has been freed. Leave it 0 to let Resolve compute purely
// from live FreeBytes/ModelMaxCtx/policy. The distinct type exists so a caller
// cannot pass a bare int without a visible, greppable conversion.
type HardContextLimit int

// Params are the inputs to a capacity resolution. Zero values mean "unknown":
// an unknown ModelMaxCtx or KVBytesPerToken disables that side of the clamp
// rather than producing a bogus window.
type Params struct {
	ModelMaxCtx         int   // model's trained context ceiling (0 = unknown)
	KVBytesPerToken     int64 // 0 = unknown (cannot budget by memory)
	LayerKV             LayerKVProfile
	WeightsBytes        int64            // resident model weight footprint
	OverheadBytes       int64            // fixed runtime buffers (compute graph, staging)
	FreeBytes           int64            // device free memory
	ReservedBytes       int64            // memory already reserved by resident sessions
	UserLimitBytes      int64            // user cap for modeld resident memory (0 = no cap)
	MinFreeBytes        int64            // memory to leave free for the desktop/other workloads
	HostColdBudgetBytes int64            // host-RAM budget for cold KV blocks (0 = none)
	Request             HardContextLimit // explicit requested window (0 = use the resolved max); see HardContextLimit
	HeadroomFrac        float64          // <=0 or >=1 falls back to DefaultHeadroomFrac
}

// ModelCapacity is the resolved result reported to the runtime. EffectiveContext
// remains the dense context window modeld will actually serve today and the
// value the cache identity must use. MemoryContextTokens is the raw KV-token
// budget from memory before model/request clamping. HotContextTokens is the
// physical hot KV budget. PlannerEffectiveContext is the logical planner window:
// it equals the dense window when no host cold budget exists, and can grow by the
// cold KV token budget once host offload is configured.
type ModelCapacity struct {
	ModelMaxContext         int
	EffectiveContext        int
	MemoryContextTokens     int
	HotContextTokens        int
	PlannerEffectiveContext int
	KVBytesPerToken         int64
	FreeBytes               int64
	WeightsBytes            int64
	OverheadBytes           int64
	ReservedBytes           int64
	UserLimitBytes          int64
	MinFreeBytes            int64
	HostColdBudgetBytes     int64
	UsableBytes             int64
	RequiredBytes           int64
	Clamped                 bool
	Reason                  string
}

// Resolve computes the dense compatibility window, physical hot context budget,
// and logical planner window:
//
//	usable = min(free - minFree, userLimit - reserved) * (1 - headroom)
//	effective = clamp(request, 0, min(modelMax, (usable - weights - overhead) / kvBytesPerToken))
//
// Unknown inputs degrade gracefully: with no KV cost it falls back to the model
// ceiling (clamped by request); with no ceiling it uses the memory budget.
func Resolve(p Params) ModelCapacity {
	request := int(p.Request)
	headroom := p.HeadroomFrac
	if headroom <= 0 || headroom >= 1 {
		headroom = DefaultHeadroomFrac
	}

	eff := p.ModelMaxCtx // may be 0 = unknown ceiling
	usable := max(p.FreeBytes-p.MinFreeBytes, 0)
	if p.UserLimitBytes > 0 {
		usable = min(usable, max(p.UserLimitBytes-p.ReservedBytes, 0))
	}
	usable = max(int64(float64(usable)*(1-headroom)), 0)

	kvBudget := p.KVBytesPerToken
	if kvBudget <= 0 && p.LayerKV.Valid() {
		kvBudget = p.LayerKV.DenseKVBytesPerToken()
	}

	memoryTokens := 0
	if kvBudget > 0 {
		budget := max(usable-p.WeightsBytes-p.OverheadBytes, 0)
		if p.LayerKV.Valid() {
			memoryTokens = p.LayerKV.tokensForBudget(budget, max(p.ModelMaxCtx, request))
		} else {
			memoryTokens = int(budget / kvBudget)
		}
		if eff <= 0 || memoryTokens < eff {
			eff = memoryTokens
		}
	}

	if request > 0 {
		switch {
		case eff > 0 && request < eff:
			eff = request
		case eff <= 0 && kvBudget <= 0 && p.ModelMaxCtx <= 0:
			eff = request
		}
	}
	if eff < 0 {
		eff = 0
	}

	requestForRequired := request
	if requestForRequired <= 0 {
		requestForRequired = eff
	}
	required := p.WeightsBytes + p.OverheadBytes
	if kvBudget > 0 && requestForRequired > 0 {
		if p.LayerKV.Valid() {
			required += p.LayerKV.KVBytesForContext(requestForRequired)
		} else {
			required += int64(requestForRequired) * kvBudget
		}
	}
	clamped, reason := false, ""
	switch {
	case p.UserLimitBytes > 0 && required > p.UserLimitBytes:
		clamped, reason = true, "request_exceeds_user_limit"
	case p.MinFreeBytes > 0 && p.FreeBytes <= p.MinFreeBytes:
		clamped, reason = true, "device_free_memory_below_reserve"
	case request > 0 && eff < request:
		clamped, reason = true, "request_exceeds_memory_budget"
	case request <= 0 && p.ModelMaxCtx > 0 && eff < p.ModelMaxCtx:
		clamped, reason = true, "model_context_exceeds_memory_budget"
	}

	hotTokens := eff
	if kvBudget > 0 && memoryTokens > 0 {
		hotTokens = memoryTokens
		if p.ModelMaxCtx > 0 && hotTokens > p.ModelMaxCtx {
			hotTokens = p.ModelMaxCtx
		}
		if request > 0 && hotTokens > request {
			hotTokens = request
		}
	}
	coldTokens := 0
	if kvBudget > 0 && p.HostColdBudgetBytes > 0 {
		coldTokens = int(p.HostColdBudgetBytes / kvBudget)
	}
	planner := hotTokens + coldTokens
	if p.ModelMaxCtx > 0 && planner > p.ModelMaxCtx {
		planner = p.ModelMaxCtx
	}
	if request > 0 && planner > request {
		planner = request
	}
	if planner < eff {
		planner = eff
	}

	return ModelCapacity{
		ModelMaxContext:         p.ModelMaxCtx,
		EffectiveContext:        eff,
		MemoryContextTokens:     memoryTokens,
		HotContextTokens:        hotTokens,
		PlannerEffectiveContext: planner,
		KVBytesPerToken:         kvBudget,
		FreeBytes:               p.FreeBytes,
		WeightsBytes:            p.WeightsBytes,
		OverheadBytes:           p.OverheadBytes,
		ReservedBytes:           p.ReservedBytes,
		UserLimitBytes:          p.UserLimitBytes,
		MinFreeBytes:            p.MinFreeBytes,
		HostColdBudgetBytes:     p.HostColdBudgetBytes,
		UsableBytes:             usable,
		RequiredBytes:           required,
		Clamped:                 clamped,
		Reason:                  reason,
	}
}

// Policy is the user/operator memory policy modeld applies before opening a
// resident session. MaxResidentBytes is a hard ceiling on modeld's resident
// footprint for the served device; MinFreeBytes preserves memory for the desktop
// or other local workloads that may share the same device.
type Policy struct {
	MaxResidentBytes    int64   `json:"max_resident_bytes,omitempty"`
	MinFreeBytes        int64   `json:"min_free_bytes,omitempty"`
	HostColdBudgetBytes int64   `json:"host_cold_budget_bytes,omitempty"`
	MinHotContextTokens int     `json:"min_hot_context_tokens,omitempty"` // 0 = backend default, <0 = explicitly disabled
	HeadroomFrac        float64 `json:"headroom_frac,omitempty"`
}

// WithResidentDefault fills a missing resident-memory cap from the device's
// CURRENT free memory. Services call it with a fresh snapshot on every
// resolution, so the default tracks the device live — it rises when memory frees
// up and falls when other workloads claim it. An explicit MaxResidentBytes (the
// user's hard cap) always wins and is left untouched. On accelerator devices it
// also fills a missing MinFreeBytes reserve floor (see
// DefaultAcceleratorMinFreeBytes) so other GPU clients keep working memory; an
// explicit MinFreeBytes always wins.
func WithResidentDefault(p Policy, dev DeviceSnapshot) Policy {
	if p.MinFreeBytes <= 0 && dev.IsAccelerator() {
		reserve := DefaultAcceleratorMinFreeBytes
		if byFrac := int64(float64(dev.TotalBytes) * DefaultAcceleratorMinFreeFrac); byFrac > reserve {
			reserve = byFrac
		}
		// Never reserve more than a quarter of a known device: the floor
		// protects co-tenants, it must not make small devices unusable.
		if capMax := dev.TotalBytes / 4; capMax > 0 && reserve > capMax {
			reserve = capMax
		}
		p.MinFreeBytes = reserve
	}
	if p.MaxResidentBytes <= 0 && dev.FreeBytes > 0 {
		p.MaxResidentBytes = int64(float64(dev.FreeBytes) * DefaultMaxResidentFrac)
	}
	return p
}

// WithHostColdDefaults fills the host-RAM cold-store budget from a host memory
// snapshot. It is separate from WithResidentDefault because the hot model budget
// may come from VRAM while the cold store always lives in host RAM.
func WithHostColdDefaults(p Policy, host DeviceSnapshot) Policy {
	if p.HostColdBudgetBytes <= 0 && host.FreeBytes > 0 {
		p.HostColdBudgetBytes = int64(float64(host.FreeBytes) * DefaultHostColdFrac)
	}
	return p
}

// LoadPolicy reads <dataRoot>/modeld.json and then applies env overrides. The
// JSON accepts either numeric byte fields or string fields ("8GiB", "512MiB"):
//
//	{"memory":{"max_resident":"8GiB","reserve_free":"2GiB","headroom_frac":0.15}}
func LoadPolicy(dataRoot string) Policy {
	var p Policy
	if dataRoot != "" {
		var raw struct {
			Memory struct {
				MaxResidentBytes    int64   `json:"max_resident_bytes"`
				MinFreeBytes        int64   `json:"min_free_bytes"`
				HostColdBudgetBytes int64   `json:"host_cold_budget_bytes"`
				MinHotContextTokens *int    `json:"min_hot_context_tokens"`
				MaxResident         string  `json:"max_resident"`
				ReserveFree         string  `json:"reserve_free"`
				HostColdBudget      string  `json:"host_cold_budget"`
				HeadroomFrac        float64 `json:"headroom_frac"`
			} `json:"memory"`
		}
		path := dataRoot + string(os.PathSeparator) + "modeld.json"
		if b, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(b, &raw); err != nil {
				slog.Warn("modeld.json is malformed; memory policy falls back to defaults", "path", path, "err", err)
			}
			p.MaxResidentBytes = raw.Memory.MaxResidentBytes
			p.MinFreeBytes = raw.Memory.MinFreeBytes
			p.HostColdBudgetBytes = raw.Memory.HostColdBudgetBytes
			if raw.Memory.MinHotContextTokens != nil {
				if *raw.Memory.MinHotContextTokens <= 0 {
					p.MinHotContextTokens = -1 // explicit off; zero value means unset/default.
				} else {
					p.MinHotContextTokens = *raw.Memory.MinHotContextTokens
				}
			}
			p.HeadroomFrac = raw.Memory.HeadroomFrac
			applyBytesSetting("modeld.json memory.max_resident", raw.Memory.MaxResident, &p.MaxResidentBytes)
			applyBytesSetting("modeld.json memory.reserve_free", raw.Memory.ReserveFree, &p.MinFreeBytes)
			applyBytesSetting("modeld.json memory.host_cold_budget", raw.Memory.HostColdBudget, &p.HostColdBudgetBytes)
		}
	}
	applyBytesSetting("CONTENOX_MODELD_MEM_MAX", os.Getenv("CONTENOX_MODELD_MEM_MAX"), &p.MaxResidentBytes)
	applyBytesSetting("CONTENOX_MODELD_MEM_RESERVE", os.Getenv("CONTENOX_MODELD_MEM_RESERVE"), &p.MinFreeBytes)
	applyBytesSetting("CONTENOX_MODELD_MEM_COLD", os.Getenv("CONTENOX_MODELD_MEM_COLD"), &p.HostColdBudgetBytes)
	if v := os.Getenv("CONTENOX_MODELD_MEM_HEADROOM"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			p.HeadroomFrac = f
		} else {
			slog.Warn("memory setting ignored: headroom must be a fraction in (0,1)", "setting", "CONTENOX_MODELD_MEM_HEADROOM", "value", v)
		}
	}
	if v := os.Getenv("CONTENOX_MODELD_MIN_HOT_CONTEXT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			if n == 0 {
				p.MinHotContextTokens = -1 // explicit off; zero value means unset/default.
			} else {
				p.MinHotContextTokens = n
			}
		} else {
			slog.Warn("memory setting ignored: min hot context must be a non-negative integer", "setting", "CONTENOX_MODELD_MIN_HOT_CONTEXT", "value", v)
		}
	}
	return p
}

// applyBytesSetting applies one byte-size policy setting, warning instead of
// silently ignoring an invalid value: a typo'd "8GBB" degrading quietly to a
// default budget is exactly the failure mode a memory policy must not have.
// Empty raw means the setting is absent and dst is left untouched.
func applyBytesSetting(name, raw string, dst *int64) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	v, err := ParseBytes(raw)
	if err != nil {
		slog.Warn("memory setting ignored: invalid byte size", "setting", name, "value", raw, "err", err)
		return
	}
	if v > 0 {
		*dst = v
	}
}

// ParseBytes parses byte strings used by modeld memory settings.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	lower := strings.ToLower(s)
	mult := int64(1)
	for _, suffix := range []struct {
		s string
		m int64
	}{
		{"gib", 1 << 30}, {"gb", 1000 * 1000 * 1000}, {"gi", 1 << 30}, {"g", 1000 * 1000 * 1000},
		{"mib", 1 << 20}, {"mb", 1000 * 1000}, {"mi", 1 << 20}, {"m", 1000 * 1000},
		{"kib", 1 << 10}, {"kb", 1000}, {"ki", 1 << 10}, {"k", 1000},
		{"b", 1},
	} {
		if strings.HasSuffix(lower, suffix.s) {
			mult = suffix.m
			s = strings.TrimSpace(s[:len(s)-len(suffix.s)])
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	return int64(n * float64(mult)), nil
}

// HeadroomFromEnv reads CONTENOX_MODELD_MEM_HEADROOM (a fraction in (0,1)),
// falling back to DefaultHeadroomFrac.
func HeadroomFromEnv() float64 {
	if v := os.Getenv("CONTENOX_MODELD_MEM_HEADROOM"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			return f
		}
	}
	return DefaultHeadroomFrac
}

// MemorySource reports the free memory of the device a backend serves on. modeld
// picks the source by device: system RAM for CPU; GPU VRAM (ov::Core / ggml) is a
// CGO seam filled per backend when a GPU device is selected.
type MemorySource interface {
	FreeBytes() (int64, error)
}

// DeviceSnapshot describes the memory pool the backend will allocate from.
type DeviceSnapshot struct {
	Kind              string `json:"kind,omitempty"`
	DeviceID          string `json:"device_id,omitempty"`
	TotalBytes        int64  `json:"total_bytes,omitempty"`
	FreeBytes         int64  `json:"free_bytes,omitempty"`
	SharedWithDisplay bool   `json:"shared_with_display,omitempty"`
}

// IsAccelerator reports whether the snapshot describes a discrete/integrated
// accelerator (as opposed to system RAM), i.e. a device other processes share
// via driver contexts and that deserves a free-memory reserve floor.
func (d DeviceSnapshot) IsAccelerator() bool {
	switch strings.ToLower(strings.TrimSpace(d.Kind)) {
	case "gpu", "igpu", "accel":
		return true
	default:
		return false
	}
}

// SystemRAM reports available host RAM via gopsutil — the CPU-device source.
type SystemRAM struct{}

func (SystemRAM) FreeBytes() (int64, error) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}
	return int64(v.Available), nil
}

func (SystemRAM) Snapshot() (DeviceSnapshot, error) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return DeviceSnapshot{}, err
	}
	return DeviceSnapshot{
		Kind:       "system",
		DeviceID:   "ram",
		TotalBytes: int64(v.Total),
		FreeBytes:  int64(v.Available),
	}, nil
}

// Snapshot returns a DeviceSnapshot for either a richer source with Snapshot or
// a legacy FreeBytes-only source.
func Snapshot(src MemorySource) (DeviceSnapshot, error) {
	if src == nil {
		src = SystemRAM{}
	}
	if s, ok := src.(interface {
		Snapshot() (DeviceSnapshot, error)
	}); ok {
		return s.Snapshot()
	}
	free, err := src.FreeBytes()
	if err != nil {
		return DeviceSnapshot{}, err
	}
	return DeviceSnapshot{Kind: "unknown", FreeBytes: free}, nil
}
