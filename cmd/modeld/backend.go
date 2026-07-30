package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/libtransport"
	"github.com/contenox/contenox/libtracker"
)

// backendFactory builds the transport.Service for one compiled-in local backend.
type backendFactory func(capacity.Policy, libtracker.ActivityTracker) transport.Service

// backends holds every inference backend compiled into this build, keyed by
// name. Backend files register themselves from init() under their build tags
// (see backend_llama.go, backend_openvino.go); an untagged build registers
// nothing and selectBackend falls back to unavailableBackend.
var backends = map[string]backendFactory{}

// backendHasAccel holds the optional accelerator probe for each compiled-in
// backend, keyed by name. A backend registers a probe (from its init()) when it
// can report whether an accelerator device is actually present on this host;
// selectBackend uses it to prefer an accelerated backend when several are
// compiled in. A nil probe (or absent entry) means "no accelerator known".
var backendHasAccel = map[string]func() bool{}

// backendDiagnostics holds optional, human-facing runtime/device inventory for
// a compiled-in backend. It is deliberately diagnostic only; backend selection
// still works with just backendHasAccel for tests and minimal builds.
var backendDiagnostics = map[string]func() backendDiagnostic{}

// backendBuildInfo holds optional static build metadata for a compiled-in
// backend (e.g. the pinned native source version), keyed by backend name then
// field. Registered from backend init()s. Unlike backendDiagnostics it must stay
// cheap and side-effect free — no native lib load, no device probe — so
// `modeld version` can report it for a release smoke check.
var backendBuildInfo = map[string]map[string]string{}

// registerBackendBuildInfo records static build metadata for a compiled-in
// backend. Called from backend init()s alongside registerBackend.
func registerBackendBuildInfo(name string, info map[string]string) {
	if len(info) == 0 {
		return
	}
	backendBuildInfo[name] = info
}

type backendDiagnostic struct {
	RuntimeName        string
	RuntimeDigest      string
	RuntimeSystemInfo  string
	SupportsGPUOffload bool
	Devices            []transport.DeviceInfo
}

type backendProbeResult struct {
	Name        string
	Accelerated bool
	Diagnostic  backendDiagnostic
}

// registerBackend records a compiled-in backend. Called from backend file
// init()s; the build tags decide which backends a given binary contains.
// hasAccel is an optional probe (may be nil) reporting whether this backend has
// an accelerator on the current host.
func registerBackend(name string, f backendFactory, hasAccel func() bool, diagnostics ...func() backendDiagnostic) {
	if _, dup := backends[name]; dup {
		panic("modeld: backend registered twice: " + name)
	}
	backends[name] = f
	if hasAccel != nil {
		backendHasAccel[name] = hasAccel
	}
	if len(diagnostics) > 0 && diagnostics[0] != nil {
		backendDiagnostics[name] = diagnostics[0]
	}
}

// preferredBackendOrder breaks ties when several backends are compiled in and
// no backend reports an accelerator. Override per process with
// CONTENOX_MODELD_BACKEND.
var preferredBackendOrder = []string{"openvino", "llama"}

// preferredAcceleratedBackendOrder breaks ties when several compiled-in
// backends report an accelerator. In the direct llama.cpp build, "llama"
// acceleration means ggml loaded a non-CPU plugin (CUDA on the dev/runtime
// target), so prefer that path over OpenVINO when both see the same dGPU.
var preferredAcceleratedBackendOrder = []string{"llama", "openvino"}

// selectBackend chooses the transport.Service this daemon serves. Selection:
//  1. CONTENOX_MODELD_BACKEND, if set and compiled in;
//  2. the only compiled-in backend, if exactly one;
//  3. among several, a backend reporting an accelerator on this host wins, with
//     preferredAcceleratedBackendOrder breaking remaining ties;
//  4. unavailableBackend when none is compiled in — the daemon still owns the
//     lease and answers health probes, it just cannot open sessions.
func selectBackend(policy capacity.Policy, tracker libtracker.ActivityTracker) (transport.Service, string) {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	if want := os.Getenv("CONTENOX_MODELD_BACKEND"); want != "" {
		if f, ok := backends[want]; ok {
			fmt.Fprintf(os.Stderr, "modeld backend selected: backend=%s reason=env_override env=CONTENOX_MODELD_BACKEND compiled=%s\n", want, availableBackends())
			return f(policy, tracker), want
		}
		fmt.Fprintf(os.Stderr, "modeld: CONTENOX_MODELD_BACKEND=%q is not compiled into this build (have: %s); falling back\n", want, availableBackends())
	}

	names := availableBackendNames()
	switch len(names) {
	case 0:
		fmt.Fprintln(os.Stderr, "modeld backend selected: backend=none reason=no_compiled_backend")
		return unavailableBackend{}, "none"
	case 1:
		fmt.Fprintf(os.Stderr, "modeld backend selected: backend=%s reason=only_compiled_backend\n", names[0])
		return backends[names[0]](policy, tracker), names[0]
	}

	// Several backends compiled in and no override: prefer one that reports an
	// accelerator present on this host, so a universal binary uses the GPU path
	// that physically exists rather than the static preference. Probing loads the
	// backend's native libs (CUDA / OpenVINO) once at startup.
	candidates := names
	preference := preferredBackendOrder
	var accelerated []string
	probes := collectBackendProbes(names)
	for _, probe := range probes {
		logBackendProbe(probe)
		if probe.Accelerated {
			accelerated = append(accelerated, probe.Name)
		}
	}
	if len(accelerated) > 0 {
		candidates = accelerated
		preference = preferredAcceleratedBackendOrder
	}

	chosen := choosePreferredBackend(candidates, preference)
	if len(accelerated) > 0 {
		reason := "accelerated_backend"
		if len(accelerated) > 1 {
			reason = "accelerated_tie"
		}
		fmt.Fprintf(os.Stderr, "modeld backend selected: backend=%s reason=%s compiled=%s accelerated=%s preference=%s override_hint=CONTENOX_MODELD_BACKEND\n",
			chosen, reason, availableBackends(), strings.Join(accelerated, ","), strings.Join(preference, ","))
	} else {
		fmt.Fprintf(os.Stderr, "modeld backend selected: backend=%s reason=no_accelerator_detected compiled=%s preference=%s override_hint=CONTENOX_MODELD_BACKEND\n",
			chosen, availableBackends(), strings.Join(preference, ","))
	}
	return backends[chosen](policy, tracker), chosen
}

func collectBackendProbes(names []string) []backendProbeResult {
	out := make([]backendProbeResult, 0, len(names))
	for _, name := range names {
		out = append(out, probeBackend(name))
	}
	return out
}

func probeBackend(name string) backendProbeResult {
	res := backendProbeResult{Name: name}
	if diagnostics := backendDiagnostics[name]; diagnostics != nil {
		res.Diagnostic = diagnostics()
		res.Accelerated = diagnosticHasAccelerator(res.Diagnostic)
	}
	if res.Accelerated {
		return res
	}
	if probe := backendHasAccel[name]; probe != nil {
		res.Accelerated = probe()
	}
	return res
}

func backendDiagnosticFromModelInfo(info transport.ModelInfo) backendDiagnostic {
	return backendDiagnostic{
		RuntimeName:        info.RuntimeName,
		RuntimeDigest:      info.RuntimeDigest,
		RuntimeSystemInfo:  info.RuntimeSystemInfo,
		SupportsGPUOffload: info.SupportsGPUOffload,
		Devices:            info.Devices,
	}
}

func diagnosticHasAccelerator(d backendDiagnostic) bool {
	for _, device := range d.Devices {
		if isAcceleratorType(device.Type) {
			return true
		}
	}
	return false
}

func isAcceleratorType(deviceType string) bool {
	switch strings.ToLower(strings.TrimSpace(deviceType)) {
	case "gpu", "igpu", "accel":
		return true
	default:
		return false
	}
}

func logBackendProbe(probe backendProbeResult) {
	d := probe.Diagnostic
	fmt.Fprintf(os.Stderr, "modeld backend probe: backend=%s accelerated=%t runtime=%s digest=%s supports_gpu_offload=%t devices=%s\n",
		probe.Name,
		probe.Accelerated,
		quoteLogValue(d.RuntimeName),
		quoteLogValue(shortDigest(d.RuntimeDigest)),
		d.SupportsGPUOffload,
		formatDeviceList(d.Devices),
	)
	if d.RuntimeSystemInfo != "" {
		fmt.Fprintf(os.Stderr, "modeld backend system-info: backend=%s %s\n", probe.Name, d.RuntimeSystemInfo)
	}
}

func choosePreferredBackend(candidates, preference []string) string {
	chosen := candidates[0] // deterministic fallback if none is in the preference list
	for _, name := range preference {
		if slices.Contains(candidates, name) {
			chosen = name
			break
		}
	}
	return chosen
}

// availableBackendNames returns the registered backend names, sorted for
// deterministic selection and logging.
func availableBackendNames() []string {
	names := make([]string, 0, len(backends))
	for name := range backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func availableBackends() string {
	names := availableBackendNames()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// errNoBackend is returned by OpenSession when this modeld build has no local
// inference backend compiled in.
var errNoBackend = errors.New("modeld: no local inference backend compiled in this build (build with -tags 'openvino openvino_genai' or -tags llamanode)")

type unavailableBackend struct{}

func (unavailableBackend) OpenSession(context.Context, transport.OpenSessionRequest) (transport.Session, error) {
	return nil, errNoBackend
}

func (unavailableBackend) Describe(context.Context, transport.OpenSessionRequest) (transport.ModelInfo, error) {
	return transport.ModelInfo{}, errNoBackend
}

func (unavailableBackend) Embed(context.Context, transport.EmbedRequest) (transport.EmbedResult, error) {
	return transport.EmbedResult{}, errNoBackend
}
