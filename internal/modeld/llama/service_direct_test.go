//go:build llamacpp_direct

package llama

import (
	"testing"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/transport"
)

func TestUnit_ServiceDescribeReportsDirectRuntime(t *testing.T) {
	path := writeTestGGUF(t, 32768)
	svc := NewService(
		WithMemorySource(staticMemory(16<<30)),
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
	if info.RuntimeName != "llama.cpp" {
		t.Fatalf("RuntimeName = %q, want llama.cpp", info.RuntimeName)
	}
	if info.RuntimeSystemInfo == "" {
		t.Fatal("RuntimeSystemInfo is empty")
	}
	if len(info.Devices) == 0 {
		t.Fatal("expected at least one registered ggml device")
	}
}
