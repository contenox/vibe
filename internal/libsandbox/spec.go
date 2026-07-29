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

// ErrIsolation is returned when building or enforcing the wall itself fails
// (unresolvable carve-out path, unserializable plan, or a shim/syscall
// failure). The wall is fail-closed: on this error the agent is not spawned.
var ErrIsolation = errors.New("libsandbox: isolation failure")

// Spec is the confinement spec for one spawned agent: the whole surface the
// process is allowed to touch. Anything not named here is absent once the
// wall is enforced; the default answer to "add a field?" is no.
type Spec struct {
	// WorkspaceRoot is the one writable directory the agent works in
	// (pinned via cmd.Dir).
	WorkspaceRoot string

	// Home is the scoped HOME for this run, so the agent's tools resolve ~/…
	// here instead of the operator's real home; carve-outs bind under it
	// while ~/.ssh, ~/.aws, ~/.npmrc etc. stay unreachable.
	Home string

	// EnvAllow lists names of parent environment variables passed through
	// unchanged. Everything else, including any inherited credential, is
	// dropped by default.
	EnvAllow []string

	// EnvSet holds explicit extra variables, overriding same-named EnvAllow
	// values. HOME cannot be set here — it is always forced to Home (see
	// scrubEnv).
	EnvSet map[string]string

	// FS lists filesystem carve-outs outside the workspace. Empty means the
	// workspace is the only reachable path.
	FS []FSCarveout

	// Net lists network carve-outs. Empty means no route.
	Net []NetCarveout

	// AllowPrivateEgress opts into dialing carve-out hosts that resolve to
	// non-public addresses (loopback, link-local incl. cloud metadata,
	// RFC1918/ULA, unspecified, multicast). Default false is SSRF-safe since
	// the parent dials from the host namespace. Requires NetworkWall; does
	// not widen which hosts are reachable, only whether resolution may go
	// inward.
	AllowPrivateEgress bool

	// NetworkWall opts into the namespaced network wall (routeless netns
	// plus metered per-host egress for Net carve-outs), built via an
	// unprivileged user namespace — where the host can't build one, Command
	// fails closed rather than run with the network open.
	//
	// Default off: confining a subprocess must not hinge on a kernel
	// capability the host may withhold. With it off, Landlock and the scoped
	// HOME still confine filesystem/exec/environment; only the network stays
	// open. A spec naming Net or AllowPrivateEgress with NetworkWall off is
	// rejected rather than silently leaving the network open.
	NetworkWall bool

	// Tracker observes the wall (Command's assembly lifecycle, and later
	// every blocked bypass attempt). Nil is treated as libtracker.NoopTracker.
	Tracker libtracker.ActivityTracker

	// SyscallTap opts into a seccomp user-notify telemetry tap (Linux only,
	// default off): execve/execveat attempts are reported to the Tracker and
	// then always allowed to continue — it observes, it does not decide.
	// Enforcement stays with Landlock/netns regardless. Off Linux it is inert.
	SyscallTap bool
}

// FSCarveout is one hole in the filesystem wall: a path, the access granted,
// and — mandatory — why it's needed. Also the on-disk shape of a filesystem
// necessity-list entry.
type FSCarveout struct {
	// Path is the filesystem path to make reachable (e.g. "~/.claude"). Must be
	// non-empty and must not contain a ".." traversal segment.
	Path string `json:"path"`
	// Mode is ModeRO or ModeRW.
	Mode string `json:"mode"`
	// Needs states, in plain words, why the agent breaks without this path.
	Needs string `json:"needs"`
}

// NetCarveout is one hole in the network wall: a host, why it's needed, and
// optionally which ports. Also the on-disk shape of a network necessity-list
// entry.
type NetCarveout struct {
	// Host is the network host to allow egress to (e.g. "registry.npmjs.org").
	Host string `json:"host"`
	// Ports narrows the hole to specific destination ports; empty allows every
	// port. A connection to any other port on the same host is refused and
	// logged like a non-carve-out host. Each entry must be a valid TCP port
	// (1-65535).
	Ports []int `json:"ports,omitempty"`
	// Needs states why the agent breaks without reaching this host.
	Needs string `json:"needs"`
}

// validate reports whether the spec is usable and every carve-out well-formed.
// Structural failures wrap ErrInvalidSpec; malformed carve-outs wrap
// ErrInvalidCarveout, so callers can tell "there is no workspace" from "one
// hole is bad".
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

// validateFSCarveout returns a plain (unwrapped) reason a filesystem carve-out
// is malformed, or nil. Call sites wrap it with ErrInvalidCarveout and a
// position, so the same rule serves both LoadCarveouts and Spec.validate.
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

// validateNetCarveout returns a plain (unwrapped) reason a network carve-out is
// malformed, or nil. See validateFSCarveout for the wrapping contract.
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

// hasTraversal reports whether p has a ".." path segment (checked
// segment-wise, not as a substring, so "foo..bar" is not a false positive).
func hasTraversal(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
