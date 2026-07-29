//go:build llamacpp_direct

package llama

import (
	"fmt"
	"os"
	"strings"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
	"github.com/contenox/contenox/internal/transport"
)

func init() {
	defaultMemorySource = func(cfg transport.Config) capacity.MemorySource {
		if forceCPUFromEnv() {
			return capacity.SystemRAM{}
		}
		return ggmlDeviceMemorySource{requireGPU: false}
	}
	llamaRuntimeInfo = directRuntimeInfo
}

func forceCPUFromEnv() bool {
	v, ok := os.LookupEnv("CONTENOX_LLAMA_GPU_LAYERS")
	return ok && strings.TrimSpace(v) == "0"
}

type ggmlDeviceMemorySource struct {
	requireGPU bool
}

func (s ggmlDeviceMemorySource) FreeBytes() (int64, error) {
	st, err := s.Snapshot()
	if err != nil {
		return 0, err
	}
	return st.FreeBytes, nil
}

func (s ggmlDeviceMemorySource) Snapshot() (capacity.DeviceSnapshot, error) {
	devices := llamacppshim.Devices()
	for _, d := range devices {
		if !isAcceleratorDevice(d.Type) {
			continue
		}
		return capacity.DeviceSnapshot{
			Kind:              d.Type,
			DeviceID:          d.Name,
			TotalBytes:        int64(d.MemoryTotal),
			FreeBytes:         int64(d.MemoryFree),
			SharedWithDisplay: d.Type == "igpu",
		}, nil
	}
	if s.requireGPU {
		return capacity.DeviceSnapshot{}, fmt.Errorf("requested GPU offload but linked llama.cpp runtime registered no GPU devices (supports_gpu_offload=%v, devices=%s)",
			llamacppshim.SupportsGPUOffload(), deviceSummary(devices))
	}
	return capacity.SystemRAM{}.Snapshot()
}

func isAcceleratorDevice(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "gpu", "igpu", "accel":
		return true
	default:
		return false
	}
}

// HasAccelerator reports whether the linked llama.cpp runtime registered a
// GPU/accelerator device on this host. modeld uses it for runtime backend
// selection; calling this loads the ggml backends once.
func HasAccelerator() bool {
	for _, d := range llamacppshim.Devices() {
		if isAcceleratorDevice(d.Type) {
			return true
		}
	}
	return false
}

func directRuntimeInfo() transport.ModelInfo {
	devices := llamacppshim.Devices()
	out := make([]transport.DeviceInfo, 0, len(devices))
	for _, d := range devices {
		out = append(out, transport.DeviceInfo{
			Index:            d.Index,
			Name:             d.Name,
			Description:      d.Description,
			Type:             d.Type,
			MemoryFree:       int64(d.MemoryFree),
			MemoryTotal:      int64(d.MemoryTotal),
			MemoryFreeKnown:  d.MemoryTotal > 0,
			MemoryTotalKnown: d.MemoryTotal > 0,
		})
	}
	return transport.ModelInfo{
		RuntimeName:        "llama.cpp",
		RuntimeDigest:      llamaCPPCommit,
		RuntimeSystemInfo:  llamacppshim.SystemInfo(),
		SupportsGPUOffload: llamacppshim.SupportsGPUOffload(),
		Devices:            out,
	}
}

func deviceSummary(devices []llamacppshim.Device) string {
	if len(devices) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(devices))
	for _, d := range devices {
		parts = append(parts, fmt.Sprintf("%d:%s:%s", d.Index, d.Type, d.Name))
	}
	return strings.Join(parts, ",")
}
