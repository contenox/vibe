package main

import (
	"testing"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/transport"
)

// TestUnit_VersionInfo locks the contract the release smoke check relies on:
// `modeld version` reports the version string (with a "dev" fallback) and the
// exact set of compiled-in backends, sorted, from the registry — never silently
// fewer than were built in.
func TestUnit_VersionInfo(t *testing.T) {
	origBackends := backends
	origBuildInfo := backendBuildInfo
	origVersion := version
	t.Cleanup(func() {
		backends = origBackends
		backendBuildInfo = origBuildInfo
		version = origVersion
	})

	fakeFactory := func(capacity.Policy) transport.Service { return unavailableBackend{} }

	t.Run("reports sorted backends and stamped version", func(t *testing.T) {
		backends = map[string]backendFactory{"openvino": fakeFactory, "llama": fakeFactory}
		backendBuildInfo = map[string]map[string]string{
			"llama": {"llama_cpp_commit": "abc123"},
		}
		version = "v1.2.3"

		got := collectVersionInfo()
		if got.Version != "v1.2.3" {
			t.Fatalf("Version = %q, want v1.2.3", got.Version)
		}
		if got.Protocol != transport.ProtocolVersion {
			t.Fatalf("Protocol = %d, want %d", got.Protocol, transport.ProtocolVersion)
		}
		if want := []string{"llama", "openvino"}; !equalStrings(got.Backends, want) {
			t.Fatalf("Backends = %v, want %v", got.Backends, want)
		}
		if got.BackendInfo["llama"]["llama_cpp_commit"] != "abc123" {
			t.Fatalf("BackendInfo missing llama commit: %v", got.BackendInfo)
		}
	})

	t.Run("no backends yields dev version and non-nil empty list", func(t *testing.T) {
		backends = map[string]backendFactory{}
		backendBuildInfo = map[string]map[string]string{}
		version = ""

		got := collectVersionInfo()
		if got.Version != "dev" {
			t.Fatalf("Version = %q, want dev", got.Version)
		}
		if got.Protocol != transport.ProtocolVersion {
			t.Fatalf("Protocol = %d, want %d", got.Protocol, transport.ProtocolVersion)
		}
		if got.Backends == nil || len(got.Backends) != 0 {
			t.Fatalf("Backends = %#v, want non-nil empty slice", got.Backends)
		}
		if got.BackendInfo != nil {
			t.Fatalf("BackendInfo = %v, want nil when empty", got.BackendInfo)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
