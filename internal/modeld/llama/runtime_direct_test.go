//go:build llamacpp_direct

package llama

import (
	"os"
	"testing"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/libtransport"
)

func TestUnit_DirectRuntimeDefaultMemorySourceUsesAcceleratorForAutoOffload(t *testing.T) {
	unsetEnvForTest(t, "CONTENOX_LLAMA_GPU_LAYERS")

	src := defaultMemorySource(transport.Config{})
	ggml, ok := src.(ggmlDeviceMemorySource)
	if !ok {
		t.Fatalf("defaultMemorySource(auto) = %T, want ggmlDeviceMemorySource", src)
	}
	if ggml.requireGPU {
		t.Fatal("auto offload should probe accelerators without hard-failing CPU-only hosts")
	}
}

func TestUnit_DirectRuntimeDefaultMemorySourceCanForceCPU(t *testing.T) {
	t.Setenv("CONTENOX_LLAMA_GPU_LAYERS", "0")

	src := defaultMemorySource(transport.Config{})
	if _, ok := src.(capacity.SystemRAM); !ok {
		t.Fatalf("defaultMemorySource(force CPU) = %T, want capacity.SystemRAM", src)
	}
}

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
			return
		}
		_ = os.Unsetenv(key)
	})
}
