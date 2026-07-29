package modeldinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/contenox/contenox/internal/transport"
)

// versionInfo mirrors the subset of `modeld version --json` we validate against.
// The full shape lives in cmd/modeld/version.go; protocol + backends are the
// compatibility contract (backend_info is ignored here).
type versionInfo struct {
	Version  string   `json:"version"`
	Protocol int      `json:"protocol"`
	Backends []string `json:"backends"`
}

const probeTimeout = 10 * time.Second

// probeBinary runs `<launcher> version --json` and parses the report. It does not
// start the daemon or claim the lease, so it is safe to run against a freshly
// extracted (or already installed) binary. A missing launcher is reported so the
// caller can decide to download.
func (c *client) probeBinary(ctx context.Context, launcher string) (versionInfo, error) {
	return ProbeBinary(ctx, launcher)
}

// ProbeBinary is the package-level form used by modeldprobe for managed-install
// discovery.
func ProbeBinary(ctx context.Context, launcher string) (versionInfo, error) {
	if !fileExists(launcher) {
		return versionInfo{}, fmt.Errorf("modeld launcher not found: %s", launcher)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, probeTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, launcher, "version", "--json")
	out, err := cmd.Output()
	if err != nil {
		return versionInfo{}, fmt.Errorf("modeld version --json: %w", err)
	}
	var raw struct {
		Version  string   `json:"version"`
		Protocol *int     `json:"protocol"`
		Backends []string `json:"backends"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return versionInfo{}, fmt.Errorf("modeld version --json: parse: %w", err)
	}
	if raw.Version == "" {
		return versionInfo{}, fmt.Errorf("modeld version --json: empty version")
	}
	// Binaries built before this field existed spoke the original transport
	// contract. Treat an absent field as the oldest supported protocol so a CLI
	// patch release can reuse already-published pre-protocol modeld artifacts.
	protocol := transport.MinProtocol
	if raw.Protocol != nil {
		protocol = *raw.Protocol
	}
	vi := versionInfo{
		Version:  raw.Version,
		Protocol: protocol,
		Backends: raw.Backends,
	}
	return vi, nil
}

// checkCapability validates a probed binary for the selected provider: its
// transport protocol must be supported and, when provider is non-empty, compiled
// backends must include that provider's backend. This is compiled-package
// capability only; live backend selection happens inside `modeld serve`.
func checkCapability(info versionInfo, provider string) error {
	if !transport.Supported(info.Protocol) {
		return fmt.Errorf("%w: binary reports protocol %d, supported window is [%d..%d]", ErrProtocolMismatch, info.Protocol, transport.MinProtocol, transport.ProtocolVersion)
	}
	if provider == "" || containsString(info.Backends, provider) {
		return nil
	}
	return fmt.Errorf("%w: provider %q needs backend %q, binary has %v", ErrBackendMissing, provider, provider, info.Backends)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
