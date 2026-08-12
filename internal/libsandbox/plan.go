package libsandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type isolationPlan struct {
	// Exec is the resolved, absolute path of the real program to run.
	Exec string `json:"exec"`
	// Args is the argv handed to Exec, unchanged from the caller.
	Args []string `json:"args"`
	// Workspace is the one read-write root: the agent's cwd, absolute.
	Workspace string `json:"workspace"`
	// Home is the scoped HOME, absolute (the anchor "~" in carve-out paths resolves against); not itself granted any access unless named as its own carve-out.
	Home string `json:"home"`
	// FS are the filesystem carve-outs with "~" already resolved against Home
	// to absolute paths, each with its access mode (ModeRO/ModeRW).
	FS []planCarveout `json:"fs"`
	// Net are the network carve-outs from Spec.Net, carried for the egress path; the fresh netns is routeless, so a carve-out alone reaches nothing yet.
	Net []NetCarveout `json:"net"`
	// Loopback asks the shim to raise "lo" inside the fresh netns (which starts down); set only when the network wall is applied.
	Loopback bool `json:"loopback"`
	// Egress asks the shim to create a TUN and hand its fd to the parent's allow-listing userspace stack, making only the Net hosts reachable; set only when Net carve-outs are declared.
	Egress bool `json:"egress,omitempty"`
	// EgressSockFD is the inherited unix socket fd the shim uses to send the TUN fd to the parent (SCM_RIGHTS) and read its readiness ack; meaningful only when Egress is set.
	EgressSockFD int `json:"egress_sock_fd,omitempty"`
	// Tap asks the shim to install a seccomp user-notify telemetry tap that reports execve/execveat and always responds CONTINUE — telemetry, never enforcement; set only when Spec.SyscallTap is on.
	Tap bool `json:"tap,omitempty"`
	// TapSockFD mirrors EgressSockFD for the seccomp notify fd; meaningful
	// only when Tap is set.
	TapSockFD int `json:"tap_sock_fd,omitempty"`
}

type planCarveout struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
}

func buildPlan(spec Spec, execPath string, args []string) (isolationPlan, error) {
	if !filepath.IsAbs(spec.WorkspaceRoot) {
		return isolationPlan{}, fmt.Errorf("%w: WorkspaceRoot %q must be an absolute path", ErrInvalidSpec, spec.WorkspaceRoot)
	}
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
		Loopback:  spec.NetworkWall,
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
