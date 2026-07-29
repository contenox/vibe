package openvino

import (
	"os"
	"strings"

	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
)

// defaultDevicePriority orders OpenVINO device categories for inference placement
// when the selector is AUTO: discrete GPU first (highest throughput), then the
// integrated GPU, then CPU as the universal fallback. Categories match
// DeviceInfo.Type from the native bridge: "gpu" = discrete GPU, "igpu" = integrated
// GPU, "accel" = NPU, "cpu" = CPU. Override with CONTENOX_OPENVINO_DEVICE_PRIORITY.
//
// The NPU ("accel") is omitted: sessions run on a ContinuousBatchingPipeline, whose
// PagedAttention op the Intel NPU compiler cannot compile, so the NPU cannot serve
// this path. An explicit CONTENOX_OPENVINO_DEVICE=NPU pin is rejected in OpenSession.
var defaultDevicePriority = []string{"gpu", "igpu", "cpu"}

// devicePriority returns the configured device-category priority order, falling
// back to the default. CONTENOX_OPENVINO_DEVICE_PRIORITY is a comma-separated
// category list (e.g. "gpu,igpu,cpu"; "npu" is accepted as an alias for
// "accel" when deliberately testing explicit NPU placement).
func devicePriority() []string {
	v := strings.TrimSpace(os.Getenv("CONTENOX_OPENVINO_DEVICE_PRIORITY"))
	if v == "" {
		return defaultDevicePriority
	}
	out := make([]string, 0, 4)
	for _, p := range strings.Split(v, ",") {
		if p = devicePriorityCategory(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return defaultDevicePriority
	}
	return out
}

func devicePriorityCategory(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "npu" {
		return "accel"
	}
	return s
}

// orderDeviceCandidates returns the concrete OpenVINO device names to attempt, in
// priority order, grouping enumerated devices by DeviceInfo.Type. Within a
// category, enumeration order is preserved. CPU is always appended as a final
// fallback because OpenVINO exposes a CPU plugin even when it is not enumerated as
// an accelerator, so a session can always be opened.
//
// Devices whose memory cannot be budgeted are excluded: capacity planning needs
// telemetry (or the shared-with-display host-RAM fallback), and a device it
// cannot budget must not be selected under AUTO. OpenVINO enumerates non-Intel
// GPUs (e.g. NVIDIA via OpenCL) without memory telemetry; selecting one fails
// every capacity probe and, through Describe, empties the model catalog.
func orderDeviceCandidates(devices []ovsession.DeviceInfo, priority []string) []string {
	if len(priority) == 0 {
		priority = defaultDevicePriority
	}
	byType := make(map[string][]string)
	for _, d := range devices {
		if !deviceBudgetable(d) {
			continue
		}
		t := strings.ToLower(strings.TrimSpace(d.Type))
		byType[t] = append(byType[t], d.Name)
	}
	out := make([]string, 0, len(devices)+1)
	seen := make(map[string]bool)
	for _, t := range priority {
		for _, name := range byType[devicePriorityCategory(t)] {
			if !seen[name] {
				out = append(out, name)
				seen[name] = true
			}
		}
	}
	hasCPU := false
	for _, name := range out {
		if openvinoDeviceBase(name) == "CPU" {
			hasCPU = true
			break
		}
	}
	if !hasCPU {
		out = append(out, "CPU")
	}
	return out
}

// deviceBudgetable mirrors openvinoDeviceSnapshot's requirements: usable memory
// telemetry, or shared-with-display (host-RAM fallback), or CPU (system RAM).
func deviceBudgetable(d ovsession.DeviceInfo) bool {
	if openvinoDeviceBase(d.Name) == "CPU" || d.SharedWithDisplay {
		return true
	}
	return d.MemoryTotalKnown && d.MemoryFreeKnown && d.MemoryTotal > 0
}

// openSessionDevices expands a session's device selector into the ordered list of
// devices to try. An explicit (non-AUTO) selector pins a single device and skips
// autodetection — the operator asked for that device specifically. AUTO triggers
// enumeration + priority ordering; if enumeration fails, CPU is the lone fallback.
func openSessionDevices(selector string, priority []string) []string {
	if openvinoDeviceBase(selector) != "AUTO" {
		return []string{selector}
	}
	info, err := ovsession.Runtime()
	if err != nil {
		return []string{"CPU"}
	}
	return orderDeviceCandidates(info.Devices, priority)
}
