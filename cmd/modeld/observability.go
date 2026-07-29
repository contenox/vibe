package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/transport"
)

// formatIdleTTL renders the idle-release TTL for logs; zero means disabled.
func formatIdleTTL(ttl time.Duration) string {
	if ttl <= 0 {
		return "off"
	}
	return ttl.String()
}

func formatPolicy(policy capacity.Policy) string {
	// Unset headroom prints as "unset", not "0.00": the effective default
	// (capacity.DefaultHeadroomFrac) is applied later inside Resolve, and
	// logging a literal zero misreads as "no headroom reserved".
	headroom := "unset"
	if policy.HeadroomFrac > 0 {
		headroom = fmt.Sprintf("%.2f", policy.HeadroomFrac)
	}
	minHot := "unset"
	if policy.MinHotContextTokens < 0 {
		minHot = "off"
	} else if policy.MinHotContextTokens > 0 {
		minHot = strconv.Itoa(policy.MinHotContextTokens)
	}
	return fmt.Sprintf("max_resident=%s reserve_free=%s host_cold=%s min_hot_context=%s headroom=%s",
		formatPolicyBytes(policy.MaxResidentBytes),
		formatPolicyBytes(policy.MinFreeBytes),
		formatPolicyBytes(policy.HostColdBudgetBytes),
		minHot,
		headroom,
	)
}

func logRuntimeEnv() {
	for _, name := range []string{
		"CONTENOX_MODELD_BACKEND",
		"CONTENOX_LLAMA_BACKEND_DIR",
		"CONTENOX_LLAMA_GPU_LAYERS",
		"CONTENOX_LLAMA_CTX",
		"CONTENOX_OPENVINO_DEVICE",
		"CONTENOX_MODELD_MEM_MAX",
		"CONTENOX_MODELD_MEM_RESERVE",
		"CONTENOX_MODELD_MEM_COLD",
		"CONTENOX_MODELD_MEM_HEADROOM",
		"CONTENOX_MODELD_MIN_HOT_CONTEXT",
		"CONTENOX_MODELD_DEVICE_LEASE_DIR",
		"CONTENOX_MODELD_IDLE_TTL",
	} {
		if value := os.Getenv(name); value != "" {
			fmt.Fprintf(os.Stderr, "modeld env: %s=%s\n", name, quoteLogValue(value))
		}
	}
}

func formatDeviceList(devices []transport.DeviceInfo) string {
	if len(devices) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(devices))
	for _, d := range devices {
		parts = append(parts, formatDevice(d))
	}
	return strings.Join(parts, ";")
}

func formatDevice(d transport.DeviceInfo) string {
	label := strings.TrimSpace(d.Name)
	desc := strings.TrimSpace(d.Description)
	if label == "" {
		label = desc
	} else if desc != "" && desc != label {
		label += " (" + desc + ")"
	}
	if label == "" {
		label = "unknown"
	}
	kind := strings.TrimSpace(d.Type)
	if kind == "" {
		kind = "unknown"
	}
	mem := ""
	if d.MemoryFree > 0 || d.MemoryTotal > 0 {
		mem = " mem=" + formatBytes(d.MemoryFree) + "/" + formatBytes(d.MemoryTotal)
	}
	return fmt.Sprintf("%d:%s:%s%s", d.Index, kind, label, mem)
}

func formatBytes(n int64) string {
	if n == 0 {
		return "0B"
	}
	if n < 0 {
		return fmt.Sprintf("%dB", n)
	}
	const unit = int64(1024)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := 0
	for n >= unit && i < len(units)-1 {
		n /= unit
		v /= float64(unit)
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", int64(v))
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

func formatPolicyBytes(n int64) string {
	if n <= 0 {
		return "unset"
	}
	return formatBytes(n)
}

func quoteLogValue(v string) string {
	if v == "" {
		return `""`
	}
	return strconv.Quote(v)
}

func shortDigest(v string) string {
	if len(v) <= 12 {
		return v
	}
	return v[:12]
}
