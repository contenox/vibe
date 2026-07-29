package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/transport"
)

// version is the modeld release version, injected at link time by the Makefile
// (-X 'main.version=vX.Y.Z'; cmd/modeld is package main, so the linker binds
// against `main`, not the full import path). A plain `go build` leaves it empty;
// resolvedVersion reports "dev" in that case.
var version string

func resolvedVersion() string {
	if version == "" {
		return "dev"
	}
	return version
}

// versionInfo is the machine-readable shape printed by `modeld version --json`.
// Backends lists the inference backends compiled into this build; the release
// smoke test asserts it matches the bundle manifest so a build can never silently
// ship fewer backends than expected.
type versionInfo struct {
	Version     string                       `json:"version"`
	Protocol    int                          `json:"protocol"`
	Backends    []string                     `json:"backends"`
	BackendInfo map[string]map[string]string `json:"backend_info,omitempty"`
}

func collectVersionInfo() versionInfo {
	names := availableBackendNames()
	if names == nil {
		names = []string{}
	}
	var info map[string]map[string]string
	if len(backendBuildInfo) > 0 {
		info = backendBuildInfo
	}
	return versionInfo{
		Version:     resolvedVersion(),
		Protocol:    transport.ProtocolVersion,
		Backends:    names,
		BackendInfo: info,
	}
}

// printVersion reports the modeld version and compiled-in backends. It does not
// load native libraries or claim the lease, so it is safe to run as a release
// smoke check against a freshly packaged binary.
func printVersion(asJSON bool) error {
	info := collectVersionInfo()
	if asJSON {
		b, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("modeld version %s\n", info.Version)
	fmt.Printf("protocol: %d\n", info.Protocol)
	if len(info.Backends) == 0 {
		fmt.Println("backends: none")
		return nil
	}
	fmt.Printf("backends: %s\n", strings.Join(info.Backends, ", "))
	for _, name := range info.Backends {
		fields := info.BackendInfo[name]
		for _, key := range sortedKeys(fields) {
			fmt.Printf("  %s: %s=%s\n", name, key, fields[key])
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
