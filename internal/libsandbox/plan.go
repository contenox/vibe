package libsandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// isolationPlan is the wall, flattened for the shim. Landlock must apply in
// the child before execve (applying it in the parent would restrict contenox
// itself), and Go cannot run code between fork and exec, so the parent
// re-execs the host binary as a shim and hands it this plan — the only thing
// crossing that boundary — serialized as JSON into a transport env var. The
// shim reads no Spec and makes no policy decision; everything it needs is here.
type isolationPlan struct {
	// Exec is the resolved, absolute path of the real program to run.
	Exec string `json:"exec"`
	// Args is the argv handed to Exec, unchanged from the caller.
	Args []string `json:"args"`
	// Workspace is the one read-write root: the agent's cwd, absolute.
	Workspace string `json:"workspace"`
	// Home is the scoped HOME, absolute: the anchor "~" in carve-out paths
	// resolves against. Not itself granted any access — a writable home must
	// be named as its own carve-out.
	Home string `json:"home"`
	// FS are the filesystem carve-outs with "~" already resolved against Home
	// to absolute paths, each with its access mode (ModeRO/ModeRW).
	FS []planCarveout `json:"fs"`
	// Net are the network carve-outs threaded through from Spec.Net. The
	// process lands in a fresh, routeless network namespace, so a carve-out
	// here reaches nothing yet; it is carried for the egress path below.
	Net []NetCarveout `json:"net"`
	// Loopback asks the shim to raise "lo" inside the fresh netns (it starts
	// down; many toolchains bind localhost to talk to their own
	// subprocesses). Set only when the Linux net wall is applied.
	Loopback bool `json:"loopback"`
	// Egress asks the shim to create a TUN in the netns and hand its fd to
	// the parent, which serves an allow-listing userspace TCP/IP stack over
	// it — the Net hosts, and only them, become reachable. Set only when the
	// spec declared Net carve-outs; otherwise the netns keeps no device/route.
	Egress bool `json:"egress,omitempty"`
	// EgressSockFD is the fd of the inherited unix socket the shim uses to
	// send the TUN fd to the parent (SCM_RIGHTS) and read its readiness ack.
	// Meaningful only when Egress is set; assigned by the parent so the shim
	// needs no knowledge of its fd layout.
	EgressSockFD int `json:"egress_sock_fd,omitempty"`
	// Tap asks the shim to install a seccomp user-notify telemetry tap: a
	// pure-Go BPF filter that reports execve/execveat via a notify fd to a
	// parent supervisor which records the attempt and always responds
	// CONTINUE — telemetry, never enforcement. Set only when Spec.SyscallTap
	// is on; independent of Egress.
	Tap bool `json:"tap,omitempty"`
	// TapSockFD mirrors EgressSockFD for the seccomp notify fd; meaningful
	// only when Tap is set.
	TapSockFD int `json:"tap_sock_fd,omitempty"`
}

// planCarveout is one resolved filesystem hole: an absolute path and its mode.
type planCarveout struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// buildPlan flattens a (validated) Spec plus the resolved target into the plan
// the shim enforces, resolving "~" and requiring absoluteness. WorkspaceRoot
// and Home must be absolute and WorkspaceRoot an existing directory, else
// ErrInvalidSpec; a carve-out path or the target that fails to resolve
// absolute wraps ErrIsolation. Assumes Spec.validate has already run
// (Command guarantees it), so modes and ".." are not re-checked here.
func buildPlan(spec Spec, execPath string, args []string) (isolationPlan, error) {
	if !filepath.IsAbs(spec.WorkspaceRoot) {
		return isolationPlan{}, fmt.Errorf("%w: WorkspaceRoot %q must be an absolute path", ErrInvalidSpec, spec.WorkspaceRoot)
	}
	// Checked here, before spawn: a chdir failure inside the forked child
	// surfaces as a misleading "fork/exec /proc/self/exe: no such file or
	// directory" instead of naming the missing workspace. The wall does not
	// create the workspace — a missing one is a caller bug.
	if info, err := os.Stat(spec.WorkspaceRoot); err != nil {
		return isolationPlan{}, fmt.Errorf("%w: WorkspaceRoot %q is not usable as the agent's working directory: %v", ErrInvalidSpec, spec.WorkspaceRoot, err)
	} else if !info.IsDir() {
		return isolationPlan{}, fmt.Errorf("%w: WorkspaceRoot %q is not a directory", ErrInvalidSpec, spec.WorkspaceRoot)
	}
	if !filepath.IsAbs(spec.Home) {
		return isolationPlan{}, fmt.Errorf("%w: Home %q must be an absolute path", ErrInvalidSpec, spec.Home)
	}
	if !filepath.IsAbs(execPath) {
		return isolationPlan{}, fmt.Errorf("%w: target %q did not resolve to an absolute path", ErrIsolation, execPath)
	}

	fs := make([]planCarveout, 0, len(spec.FS))
	for _, c := range spec.FS {
		p := resolveTilde(c.Path, spec.Home)
		if !filepath.IsAbs(p) {
			return isolationPlan{}, fmt.Errorf("%w: carve-out path %q did not resolve to an absolute path", ErrIsolation, c.Path)
		}
		fs = append(fs, planCarveout{Path: canonicalizeTopLevel(p), Mode: c.Mode})
	}

	// Copy so the plan owns the slice rather than aliasing the caller's Spec.
	net := append([]NetCarveout(nil), spec.Net...)

	return isolationPlan{
		Exec:      execPath,
		Args:      args,
		Workspace: canonicalizeTopLevel(spec.WorkspaceRoot),
		Home:      filepath.Clean(spec.Home),
		FS:        fs,
		Net:       net,
		// Meaningful only when NetworkWall is on: only then does the process
		// land in a netns whose "lo" starts down. Off, there is no netns, so
		// loopback must not be touched.
		Loopback: spec.NetworkWall,
	}, nil
}

// canonicalizeTopLevel resolves symlinks in an absolute top-level path (the
// workspace, or a "~"-resolved carve-out), pinning the real target in the
// trusted parent so the Landlock rule can't be redirected by a symlink swapped
// in at the top-level path (symlinks planted inside an already-granted
// directory are re-checked by Landlock at access time; only the anchor needs
// this). Best-effort: falls back to a lexical Clean if the path can't be
// resolved. EvalSymlinks preserves absoluteness.
func canonicalizeTopLevel(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// resolveTilde expands a leading "~" against the scoped home (see Spec.Home).
// Only "~" and "~/…" are special-cased; "~user" and other paths pass through
// unchanged.
func resolveTilde(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[len("~/"):])
	}
	return p
}
