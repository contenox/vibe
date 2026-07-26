package libsandbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/contenox/beam/internal/libtracker"
)

// Access modes for an FSCarveout. A carve-out is read-only unless the agent
// provably needs to write through it — credential and config directories are
// read, not written (see the blueprint's "read-only wherever possible").
const (
	// ModeRO grants read-only access through a filesystem carve-out.
	ModeRO = "ro"
	// ModeRW grants read-write access; use it only where a read-only hole
	// demonstrably breaks the agent.
	ModeRW = "rw"
)

// ErrInvalidSpec is returned when the confinement spec itself is unusable: no
// workspace to write in, no scoped HOME to anchor, or no command to confine.
// These are structural preconditions, distinct from a malformed carve-out.
var ErrInvalidSpec = errors.New("libsandbox: invalid spec")

// ErrInvalidCarveout is returned when a filesystem or network carve-out is
// malformed — a bad access mode, a missing justification, a traversal path, an
// empty host. A hole that cannot describe why it exists is not admitted, whether
// it came from a necessity-list file (LoadCarveouts) or a hand-built Spec.
var ErrInvalidCarveout = errors.New("libsandbox: invalid carve-out")

// ErrIsolation is returned when building or enforcing the wall itself fails,
// as distinct from a malformed spec (ErrInvalidSpec) or carve-out
// (ErrInvalidCarveout): a carve-out path that will not resolve to something
// absolute, a plan that cannot be serialized across the re-exec boundary, or —
// in the shim — a kernel without Landlock, an unopenable path, or a failed
// landlock/execve syscall. The wall is fail-closed: when it cannot be built the
// agent is not run unconfined, and the failure surfaces as ErrIsolation.
var ErrIsolation = errors.New("libsandbox: isolation failure")

// Spec is the confinement spec for one spawned agent: the whole surface the
// process is allowed to touch. Everything not named here is meant to be absent
// by construction once the wall is enforced. The fields are not a set of
// permissions granted — they are the short list of functional necessities
// without which the agent cannot boot, authenticate, or do its work. The
// default answer to "should this be in the Spec?" is no.
type Spec struct {
	// WorkspaceRoot is the one writable directory the agent works in — its
	// fork/cwd, where its edits are journaled as diffs. It is a necessity
	// because an agent with nowhere to write cannot do its job; it is also the
	// only sanctioned writable path, so the process is pinned to it (cmd.Dir).
	WorkspaceRoot string

	// Home is the scoped HOME for this run: a per-mission directory that becomes
	// $HOME, so the agent's tools resolve ~/… into here instead of the
	// operator's real home. It is the lever that reconciles "the agent needs
	// ~/.claude" with "deny the rest": the ~/.claude carve-out binds into this
	// dir, while ~/.ssh, ~/.aws, ~/.npmrc, and ~/.contenox are simply not under
	// it. It is a necessity: without a HOME the agent's toolchain misbehaves,
	// and without it being *scoped* the real home leaks.
	Home string

	// EnvAllow lists the NAMES of environment variables passed through from the
	// parent process, matched exactly. Each name is a necessity — a variable the
	// agent's toolchain reads to function (PATH to find binaries, TERM and LANG
	// for sane output). Everything else in the parent environment, every
	// inherited credential among it, is dropped. The default is to pass nothing.
	EnvAllow []string

	// EnvSet holds explicit extra variables set for the agent, overriding any
	// same-named value copied via EnvAllow. HOME is always forced to Home and
	// cannot be set here — the scoped HOME is the mechanism, not a suggestion
	// (see scrubEnv).
	EnvSet map[string]string

	// FS lists filesystem carve-outs: paths outside the workspace the agent
	// breaks without — typically its auth/config directory, read-only. Empty
	// means the workspace is the only reachable path, which is the default.
	FS []FSCarveout

	// Net lists network carve-outs: hosts the toolchain must reach or it fails,
	// such as a package registry. Empty means no route — the default.
	Net []NetCarveout

	// AllowPrivateEgress opts into dialing carve-out hosts that resolve to
	// non-public addresses — loopback, link-local (incl. 169.254.169.254 cloud
	// metadata), RFC1918/ULA private, unspecified, or multicast. Default false is
	// SSRF-safe: the parent dials the carve-out from the HOST namespace, so a
	// carve-out that resolves into the host's own network would otherwise let the
	// agent pivot onto internal services. It exists because an operator may
	// legitimately run an internal registry on a private IP; set it only then, and
	// only for the run whose carve-outs are trusted to be internal. It does not
	// widen WHICH hosts are reachable (still only Net carve-outs) — only whether a
	// carve-out is allowed to resolve inward. It requires NetworkWall (it only
	// governs how that wall resolves carve-outs).
	AllowPrivateEgress bool

	// NetworkWall opts into the namespaced network wall: a routeless network
	// namespace that makes the network absent by construction, plus the metered
	// per-host egress (the Net carve-outs) served over it. That namespace is
	// created and owned through an UNPRIVILEGED USER NAMESPACE, so it needs a host
	// where unprivileged user namespaces are permitted; where they are not (the
	// kernel knob off, or an AppArmor restriction like Ubuntu 24.04's default) the
	// wall cannot be built and Command fails closed rather than run the agent with
	// the network open.
	//
	// Default OFF, and that default is load-bearing: confining a subprocess we
	// launch ourselves must not hinge on a kernel capability the host may withhold.
	// With it off, the wall still confines the surface that matters most — the
	// filesystem and exec (Landlock) and the inherited environment (the scoped
	// HOME) — so the agent still cannot read the operator's loot paths or act
	// outside its workspace; only the network is left open, needing zero namespace
	// privilege and running anywhere. Turn it on where unprivileged userns is
	// available AND the network must be confined to a named host list. Because Net
	// carve-outs and AllowPrivateEgress are meaningless without the netns that
	// serves them, a spec that names either with NetworkWall off is rejected
	// rather than silently leaving the network open while looking confined.
	NetworkWall bool

	// Tracker observes the wall: the Command assembly lifecycle now, and in
	// later slices every blocked bypass attempt. Nil is treated as
	// libtracker.NoopTracker.
	Tracker libtracker.ActivityTracker

	// SyscallTap opts into the seccomp user-notify TELEMETRY tap (Linux only,
	// default off). The deny-by-construction floor (Landlock/netns) is
	// telemetry-poor: a denied exec is a silent EPERM the Tracker never sees. With
	// this set, a small enumerated set of syscalls (execve/execveat — "what did the
	// agent try to run") is routed to a parent supervisor that RECORDS the attempt
	// as a Tracker event and then always responds SECCOMP_USER_NOTIF_FLAG_CONTINUE,
	// so the kernel proceeds with the real syscall. It is a tap, not an enforcer: it
	// makes NO allow/deny decision (that would be the CONTINUE-plus-TOCTOU foot-gun);
	// enforcement stays with Landlock/netns, which still deny whatever they deny —
	// so a blocked attempt is both observed here and denied there, which is the
	// point. It is opt-in because it costs one userspace round-trip per tapped
	// syscall; leave it off unless an operator is watching the wall. Off Linux it is
	// inert (the whole wall is), like the rest of the mechanisms.
	SyscallTap bool
}

// FSCarveout is one hole in the filesystem wall: a path the agent needs
// reachable, the access it needs, and — mandatory — why it breaks without it.
// It doubles as the on-disk shape of a filesystem necessity-list entry.
type FSCarveout struct {
	// Path is the filesystem path to make reachable (e.g. "~/.claude"). It must
	// be non-empty and must not contain a ".." traversal segment.
	Path string `json:"path"`
	// Mode is the access granted: ModeRO or ModeRW. Read-only wherever possible;
	// credential and config directories are read, not written.
	Mode string `json:"mode"`
	// Needs states, in plain words, why the agent breaks without this path. It
	// is required: every hole must justify itself, or it should not exist.
	Needs string `json:"needs"`
}

// NetCarveout is one hole in the network wall: a host the toolchain must reach,
// and why. It doubles as the on-disk shape of a network necessity-list entry.
type NetCarveout struct {
	// Host is the network host to allow egress to (e.g. "registry.npmjs.org").
	// It is required.
	Host string `json:"host"`
	// Ports optionally narrows the hole to specific destination ports (e.g. [443]).
	// When empty (the default) the host is reachable on every port — the original,
	// host-only behaviour. When set, a connection is authorized only if its
	// destination port is in the list; every other port to the same host is
	// refused (RST) and logged, exactly like a non-carve-out host. Each entry must
	// be a valid TCP port (1-65535).
	Ports []int `json:"ports,omitempty"`
	// Needs states why the agent breaks without reaching this host. Required,
	// for the same reason FSCarveout.Needs is.
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

// hasTraversal reports whether p has a ".." path segment. It checks segments
// rather than a substring so a legitimate name like "foo..bar" is not rejected,
// while "..", "../x", "x/../y", and "~/../etc" all are. Slashes are normalized
// first so the check is uniform across the "/"-delimited paths the necessity
// list uses.
func hasTraversal(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
