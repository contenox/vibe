package libsandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// isolationPlan is the wall, flattened for the shim. Command runs in the host
// process, but Landlock must be applied in the child *before* execve (applying
// it in the parent would restrict contenox itself), and Go cannot run code
// between fork and exec — so the parent re-execs the host binary as a shim and
// hands it this plan. It is the sole thing that crosses that boundary: the real
// target to run, its argv, and the already-resolved surface the confined process
// may touch. Everything the shim needs to build the ruleset is here; it reads no
// Spec and makes no policy decision. It is serialized (JSON) into a transport
// env var; slice 3 extends it (the net carve-outs and a loopback request) without
// reshaping the seam.
type isolationPlan struct {
	// Exec is the resolved, absolute path of the real program to run once the
	// wall is up. The shim grants it read+execute (you cannot run an agent whose
	// binary you cannot execute) and then syscall.Exec's it.
	Exec string `json:"exec"`
	// Args is the argv handed to Exec, unchanged from what the caller passed to
	// Command — the confinement is transparent to the program's own view of argv.
	Args []string `json:"args"`
	// Workspace is the one read-write root: the agent's cwd, absolute.
	Workspace string `json:"workspace"`
	// Home is the scoped HOME, absolute. It is the anchor "~" in carve-out paths
	// resolves against (so the real home never leaks); it is not itself granted
	// any access here — a writable home is a carve-out the necessity list must
	// name, not an implicit hole. Carried for the record and for slice 3.
	Home string `json:"home"`
	// FS are the filesystem carve-outs with their "~" already resolved against
	// Home to absolute paths, each with its access mode (ModeRO/ModeRW).
	FS []planCarveout `json:"fs"`
	// Net are the network carve-outs threaded through from Spec.Net. They are
	// INERT in this slice: the process lands in a fresh, empty network namespace
	// with no interfaces and no route (applyIsolation clones CLONE_NEWNET), so a
	// carve-out here reaches nothing. It is carried so the future egress-proxy
	// slice — which makes the named necessity hosts reachable through a logged
	// proxy — has the host list at the enforcement layer without reshaping this
	// seam. This is the extension point that slice consumes; the shim reads Net
	// only to carry it, never to open a route.
	Net []NetCarveout `json:"net"`
	// Loopback asks the shim to raise the loopback interface inside the fresh
	// netns. An empty netns has "lo" DOWN; many toolchains bind localhost to talk
	// to their own subprocesses, so the wall brings "lo" up (the network stays
	// otherwise absent — only host-local traffic works). It is set whenever the
	// Linux net wall is applied; the shim skips the step when it is false.
	Loopback bool `json:"loopback"`
	// Egress asks the shim to build the metered egress path: create a TUN in the
	// netns and hand its fd to the parent, which serves an allow-listing userspace
	// TCP/IP stack over it (the Net hosts, and only them, become reachable through
	// logged DNS + TCP). It is set only when the spec declared Net carve-outs; with
	// none, the netns keeps the deny-by-construction floor (no device, no route)
	// and the shim skips the step. When false, Net is carried but inert, exactly as
	// before this slice.
	Egress bool `json:"egress,omitempty"`
	// EgressSockFD is the fd, in the shim, of the inherited unix socket it sends the
	// TUN fd to the parent over (SCM_RIGHTS) and reads the parent's readiness ack
	// from. Meaningful only when Egress is set. The parent assigns it (the next free
	// ExtraFiles slot) so the shim needs no knowledge of the parent's fd layout.
	EgressSockFD int `json:"egress_sock_fd,omitempty"`
	// Tap asks the shim to install the seccomp user-notify telemetry tap: a
	// pure-Go BPF filter returning SECCOMP_RET_USER_NOTIF for a small enumerated set
	// (execve/execveat) and SECCOMP_RET_ALLOW otherwise, whose notify fd is handed to
	// a parent supervisor that records each attempt and always responds CONTINUE. It
	// is set only when Spec.SyscallTap is on; with it off the shim skips the step and
	// the process runs exactly as before this slice. Independent of Egress: the tap
	// can be on with or without net carve-outs. Telemetry, never enforcement.
	Tap bool `json:"tap,omitempty"`
	// TapSockFD is the fd, in the shim, of the inherited unix socket it sends the
	// seccomp notify fd to the parent over (SCM_RIGHTS) and reads the parent's
	// readiness ack from. Meaningful only when Tap is set. The parent assigns it (the
	// next free ExtraFiles slot, after any egress slot) so the shim needs no
	// knowledge of the parent's fd layout — mirroring EgressSockFD.
	TapSockFD int `json:"tap_sock_fd,omitempty"`
}

// planCarveout is one resolved filesystem hole: an absolute path and its mode.
type planCarveout struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// buildPlan flattens a (validated) Spec plus the resolved target into the plan
// the shim enforces. It is the point where "~" is resolved and absoluteness is
// required, because the enforcement layer — not the parser (LoadCarveouts) —
// interprets paths. WorkspaceRoot and Home must be absolute (they anchor the cwd
// and "~"); a non-absolute one wraps ErrInvalidSpec, as does a WorkspaceRoot that
// is not an existing directory. Each carve-out path has "~" resolved against Home
// and must then be absolute; anything else wraps ErrIsolation, as does a target
// that did not resolve to an absolute path.
//
// It assumes Spec.validate has already run (Command guarantees it): modes are
// ModeRO/ModeRW and no path carries a ".." segment, so it only handles
// resolution and absoluteness.
func buildPlan(spec Spec, execPath string, args []string) (isolationPlan, error) {
	if !filepath.IsAbs(spec.WorkspaceRoot) {
		return isolationPlan{}, fmt.Errorf("%w: WorkspaceRoot %q must be an absolute path", ErrInvalidSpec, spec.WorkspaceRoot)
	}
	// The workspace is the child's cwd, and a chdir into a path that is not an
	// existing directory fails INSIDE the freshly forked child — where the kernel
	// reports it against the child's exec path, surfacing as the deeply misleading
	// "fork/exec /proc/self/exe: no such file or directory" (the shim binary is
	// there; the workspace is not). Check it here so the caller is told which path
	// is missing, before anything is spawned. The wall is not in the business of
	// creating it: a workspace the caller never made is a caller bug, and silently
	// materializing one would confine the agent to a directory nobody chose.
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

	// Net carve-outs need no resolution — they are (host, needs) records carried
	// verbatim for the future egress-proxy slice. Copy so the plan owns them and
	// does not alias the caller's Spec slice. In this slice they are inert: the
	// netns has no route, so nothing here is reachable until the proxy lands.
	net := append([]NetCarveout(nil), spec.Net...)

	return isolationPlan{
		Exec:      execPath,
		Args:      args,
		Workspace: canonicalizeTopLevel(spec.WorkspaceRoot),
		Home:      filepath.Clean(spec.Home),
		FS:        fs,
		Net:       net,
		// Loopback (raising "lo" inside the fresh netns) is meaningful only when the
		// namespaced network wall is on — that is the only mode that lands the process
		// in a netns whose "lo" starts DOWN. With NetworkWall off there is no netns
		// (the agent keeps the host network), so the shim must NOT touch loopback; it
		// skips the step when this is false. The plan is consumed only by the Linux
		// shim (a no-op elsewhere).
		Loopback: spec.NetworkWall,
	}, nil
}

// canonicalizeTopLevel resolves symlinks in an absolute top-level path (the
// workspace, or a "~"-resolved carve-out) so the plan records — and the Landlock
// rule is anchored to — the REAL target, pinned here in the trusted parent rather
// than followed later in the shim. This closes a symlink hazard on the top-level
// path: if the named path is (or is swapped to) a symlink into a loot directory,
// the shim would otherwise open it O_PATH — following the link — and grant the
// target; pinning the resolved path in the parent means the shim grants a fully
// canonical path with no symlink component left to redirect. Symlinks the agent
// plants *inside* an already-granted directory are a different matter and are
// re-checked by Landlock at access time; this only hardens the top-level anchor.
//
// It is best-effort: a path that does not exist (or cannot be resolved) yet has no
// symlinks to pin, so it falls back to a lexical Clean — the workspace must exist
// and will be caught by the shim's non-optional rule, and a missing carve-out is
// skipped there anyway. EvalSymlinks preserves absoluteness, so the IsAbs checks
// above still hold on the result.
func canonicalizeTopLevel(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// resolveTilde expands a leading "~" against the scoped home — the lever that
// reconciles "the agent needs ~/.claude" with "deny the rest": "~/.claude"
// binds under the per-mission home, while the operator's real ~/.ssh, ~/.aws,
// and ~/.contenox are simply not under it (see Spec.Home). Only "~" and "~/…"
// are special-cased; "~user" and already-absolute paths pass through unchanged.
func resolveTilde(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[len("~/"):])
	}
	return p
}
