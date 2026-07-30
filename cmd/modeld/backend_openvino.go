//go:build openvino && openvino_genai

package main

import (
	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/modeld/openvino"
	"github.com/contenox/contenox/internal/transport"
	"github.com/contenox/contenox/libtracker"
)

// Register the OpenVINO GenAI backend (CPU / GPU, chosen by
// CONTENOX_OPENVINO_DEVICE); selectBackend (backend.go) serves it when it is the
// only one compiled in or when CONTENOX_MODELD_BACKEND=openvino.
func init() {
	registerBackend("openvino", func(policy capacity.Policy, tracker libtracker.ActivityTracker) transport.Service {
		return openvino.NewService(openvino.WithCapacityPolicy(policy), openvino.WithTracker(tracker))
	}, openvino.HasAccelerator, func() backendDiagnostic {
		return backendDiagnosticFromModelInfo(openvino.RuntimeInfo())
	})
	registerBackendBuildInfo("openvino", map[string]string{
		"openvino_genai_version": openvino.BuildGenAIVersion(),
	})
}
