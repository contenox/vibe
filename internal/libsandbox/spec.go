package libsandbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/libtracker"
)

// Access modes for an FSCarveout.
const (
	// ModeRO grants read-only access through a filesystem carve-out.
	ModeRO = "ro"
	// ModeRW grants read-write access; use only where read-only demonstrably
	// breaks the agent.
	ModeRW = "rw"
)

// ErrInvalidSpec is returned when the spec itself is unusable: no workspace,
// no scoped HOME, or no command to confine.
var ErrInvalidSpec = errors.New("libsandbox: invalid spec")

// ErrInvalidCarveout is returned when a filesystem or network carve-out is
// malformed: bad access mode, missing justification, traversal path, or empty
// host.
var ErrInvalidCarveout = errors.New("libsandbox: invalid carve-out")

// ErrIsolation is returned when building or enforcing the wall fails (unresolvable carve-out, unserializable plan, shim/syscall failure); the wall is fail-closed, so the agent is not spawned.
var ErrIsolation = errors.New("libsandbox: isolation failure")

// Spec is the confinement spec for one spawned agent: the whole surface the process may touch; anything not named here is absent once the wall is enforced.
type Spec struct {
	// WorkspaceRoot is the one writable directory the agent works in
	// (pinned via cmd.Dir).
	WorkspaceRoot string

	// Home is the scoped HOME for this run, so the agent's tools resolve ~/…
	// here instead of the operator's real home; carve-outs bind under it
	// while ~/.ssh, ~/.aws, ~/.npmrc etc. stay unreachable.
	Home string

	// EnvAllow lists names of parent environment variables passed through unchanged; everything else, including any inherited credential, is dropped by default.
	EnvAllow []string

	// EnvSet holds explicit extra variables, overriding same-named EnvAllow values; HOME cannot be set here, it is always forced to Home.
	EnvSet map[string]string

	// FS lists filesystem carve-outs outside the workspace; empty means the workspace is the only reachable path.
	FS []FSCarveout

	// Net lists network carve-outs; empty means no route.
	Net []NetCarveout

	// AllowPrivateEgress opts into dialing carve-out hosts that resolve to non-public addresses (loopback, link-local, RFC1918/ULA, unspecified, multicast); default false is SSRF-safe, and it requires NetworkWall.
	AllowPrivateEgress bool

	// NetworkWall opts into the namespaced network wall (routeless netns plus metered per-host egress for Net carve-outs), built via an unprivileged user namespace; default off, and a spec naming Net or AllowPrivateEgress with it off is rejected.
	NetworkWall bool

	// Tracker observes the wall (Command's assembly lifecycle, and later every blocked bypass attempt); nil is treated as libtracker.NoopTracker.
	Tracker libtracker.ActivityTracker

	// SyscallTap opts into a seccomp user-notify telemetry tap (Linux only, default off) that reports execve/execveat attempts to the Tracker and always allows them to continue — it observes, it does not enforce.
	SyscallTap bool
}

// FSCarveout is one hole in the filesystem wall (a path, the access granted, and — mandatory — why it's needed), also the on-disk shape of a filesystem necessity-list entry.
type FSCarveout struct {
	// Path is the filesystem path to make reachable (e.g. "~/.claude"); must be non-empty and must not contain a ".." traversal segment.
	Path string `json:"path"`
	// Mode is ModeRO or ModeRW.
	Mode string `json:"mode"`
	// Needs states, in plain words, why the agent breaks without this path.
	Needs string `json:"needs"`
}

// NetCarveout is one hole in the network wall (a host, why it's needed, and optionally which ports), also the on-disk shape of a network necessity-list entry.
type NetCarveout struct {
	// Host is the network host to allow egress to (e.g. "registry.npmjs.org").
	Host string `json:"host"`
	// Ports narrows the hole to specific destination ports (empty allows every port, 1-65535 each); a connection to any other port on the same host is refused and logged like a non-carve-out host.
	Ports []int `json:"ports,omitempty"`
	// Needs states why the agent breaks without reaching this host.
	Needs string `json:"needs"`
}

func (s Spec) validate() error {
	if strings.TrimSpace(s.WorkspaceRoot) == "" {
		return fmt.Errorf("%w: WorkspaceRoot is required (the one writable work dir the agent is confined to)", ErrInvalidSpec)
	}
	if strings.TrimSpace(s.Home) == "" {
		return fmt.Errorf("%w: Home is required (the scoped HOME that keeps the real home out of reach)", ErrInvalidSpec)
	}
	if !s.NetworkWall {
		if len(s.Net) > 0 {
			return fmt.Errorf("%w: Net carve-outs require NetworkWall — the network wall that makes them reachable is namespaced and off by default; enable NetworkWall to confine the network, or drop the carve-outs", ErrInvalidSpec)
		}
		if s.AllowPrivateEgress {
			return fmt.Errorf("%w: AllowPrivateEgress requires NetworkWall — it only governs how the namespaced network wall resolves carve-outs", ErrInvalidSpec)
		}
	}
	for i, c := range s.FS {
		if err := validateFSCarveout(c); err != nil {
			return fmt.Errorf("%w: FS[%d]: %w", ErrInvalidCarveout, i, err)
		}
	}
	for i, c := range s.Net {
		if err := validateNetCarveout(c); err != nil {
			return fmt.Errorf("%w: Net[%d]: %w", ErrInvalidCarveout, i, err)
		}
	}
	return nil
}

func validateFSCarveout(c FSCarveout) error {
	if strings.TrimSpace(c.Path) == "" {
		return errors.New("path is required")
	}
	if hasTraversal(c.Path) {
		return fmt.Errorf("path %q must not contain a %q traversal segment", c.Path, "..")
	}
	if c.Mode != ModeRO && c.Mode != ModeRW {
		return fmt.Errorf("mode %q must be %q or %q", c.Mode, ModeRO, ModeRW)
	}
	if strings.TrimSpace(c.Needs) == "" {
		return errors.New("needs is required: every carve-out must justify why the agent breaks without it")
	}
	return nil
}

func validateNetCarveout(c NetCarveout) error {
	if strings.TrimSpace(c.Host) == "" {
		return errors.New("host is required")
	}
	if strings.TrimSpace(c.Needs) == "" {
		return errors.New("needs is required: every carve-out must justify why the agent breaks without it")
	}
	for _, port := range c.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("port %d is out of range (a TCP port must be 1-65535)", port)
		}
	}
	return nil
}

func hasTraversal(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
