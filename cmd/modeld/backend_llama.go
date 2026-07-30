//go:build llamanode && llamacpp_direct

package main

import (
	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/modeld/llama"
	// Blank-import the CGo llama.cpp adapter so its init registers the session
	// and embed factories on modeld/llama. Without this the daemon links the
	// pure-Go contract but never the backend, leaving OpenSession unavailable.
	_ "github.com/contenox/contenox/internal/modeld/llama/llamasession"
	"github.com/contenox/contenox/libtransport"
	"github.com/contenox/contenox/libtracker"
)

// Register the llama.cpp backend; selectBackend (backend.go) serves it when it
// is the only one compiled in or when CONTENOX_MODELD_BACKEND=llama.
func init() {
	registerBackend("llama", func(policy capacity.Policy, tracker libtracker.ActivityTracker) transport.Service {
		return llama.NewService(llama.WithCapacityPolicy(policy), llama.WithTracker(tracker))
	}, llama.HasAccelerator, func() backendDiagnostic {
		return backendDiagnosticFromModelInfo(llama.RuntimeInfo())
	})
	registerBackendBuildInfo("llama", map[string]string{
		"llama_cpp_commit": llama.BuildCommit(),
	})
}
