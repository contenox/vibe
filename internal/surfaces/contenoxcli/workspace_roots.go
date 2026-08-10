// workspace_roots.go is the roots source: it assembles the machine's
// workspace-root allowlist — the one list a session's working directory is
// checked against — from the launch directory, the durable grants
// `contenox workspace` writes, and this run's --workspace-root flags and
// CONTENOX_WORKSPACE_ROOTS. acpsvc advertises the result to clients and
// enforces it on every session cwd.
//
// The allowlist belongs to the machine, not to the client. A surface that
// serves remote attachments (see acp_relay.go) is reachable from a browser
// holding only a session cookie, so the directories an agent may be rooted in
// are decided here, by the operator, and a cwd outside them is refused by the
// runtime rather than negotiated with the client.
//
// # Sources, and the order they are assembled in
//
// There is one allowlist and it is a union — no source overrides or subtracts
// from another, so the only ordering that carries meaning is which root ends
// up first, because vfs.Factory hands roots[0] to a client that proposes no
// cwd or sends the "/" sentinel:
//
//  1. the launch directory (or the directory named positionally, e.g.
//     `contenox new ~/src/proj`) — always first, therefore always the default
//  2. the durable grants from `contenox workspace add`, read from the shared
//     database
//  3. --workspace-root flags, then CONTENOX_WORKSPACE_ROOTS
//
// The launch directory leads because "where the operator started the process"
// is what a session with no stated opinion should mean, and because rooting a
// session at the filesystem root instead made the agent go hunting for the
// project and trip gated shell commands. Grants precede the flags because a
// durable operator decision is the stable backdrop this run extends; between
// those two the order is presentation only (it is what DescribeRoots lists),
// since membership is a set.
//
// Union rather than override is the substantive choice: it means a flag can
// never silently widen or narrow what an operator granted, and revoking
// access is always `contenox workspace remove` rather than remembering which
// flags a launcher passed.
//
// # Grants are read once, at process start
//
// Not live. A session's cwd is fixed at session/new and immutable afterward
// (see acpsvc's workspaceRootConfigOption), so reloading the allowlist under
// a running process could only widen what a *future* session may pick — which
// the next process start already does. workspacegrants.PublishChanged rings a
// bus doorbell for this; deliberately nothing subscribes, because a live
// SetRoots would also move roots[0] and silently change the default root of a
// process already serving sessions. A new grant takes effect the next time a
// surface starts.
package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/services/workspacegrants"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

const (
	// workspaceRootFlag is the repeatable flag naming an additional directory a
	// client may root a session in, for this run only.
	workspaceRootFlag = "workspace-root"

	// workspaceRootsEnv extends the allowlist with an OS path-list separated
	// value, for launchers and service units that cannot pass flags. It is read
	// in addition to the flag, never instead of it.
	workspaceRootsEnv = "CONTENOX_WORKSPACE_ROOTS"

	workspaceRootFlagUsage = "Directory a client may root a session in, for this run (repeatable). Adds to the launch directory and the roots granted by 'contenox workspace add'; the launch directory stays the default root. Also settable via CONTENOX_WORKSPACE_ROOTS (OS path-list separated)."
)

// addWorkspaceRootFlag registers --workspace-root on a command that serves ACP
// sessions.
func addWorkspaceRootFlag(c *cobra.Command) {
	c.Flags().StringArray(workspaceRootFlag, nil, workspaceRootFlagUsage)
}

// buildWorkspaceFactory assembles the machine's workspace-root allowlist from
// every source, in the order this file documents. defaultRoot is the surface's
// launch directory and becomes the Factory's default root.
//
// store supplies the durable grants; a nil store simply contributes none, so a
// surface with no database still gets a working allowlist from its launch
// directory and flags. The result is never nil on success, so the
// workspace-root config option is always advertised on a surface that calls
// this — absent is reserved for surfaces that configure no allowlist at all
// (`contenox acp`, where the ACP protocol makes the editor supply the cwd).
//
// Duplicates across sources are collapsed by the Factory, which also resolves
// symlinks and refuses any root at or under the control plane.
//
// Two things must already have happened when a surface calls this, and beam's
// composition root satisfies both without being reordered for it: the database
// must be open, since the grants are read through it, and BuildEngine must have
// registered the control-plane denylist, since that is what the Factory screens
// roots against. Calling it earlier would silently produce an allowlist that
// screens nothing.
func buildWorkspaceFactory(cmd *cobra.Command, defaultRoot string, store runtimetypes.Store) (*vfs.Factory, error) {
	roots := []string{defaultRoot}
	roots = append(roots, readGrantedRoots(cmd, store)...)
	if cmd != nil {
		if flags, err := cmd.Flags().GetStringArray(workspaceRootFlag); err == nil {
			roots = append(roots, flags...)
		}
	}
	for _, r := range filepath.SplitList(os.Getenv(workspaceRootsEnv)) {
		if strings.TrimSpace(r) != "" {
			roots = append(roots, r)
		}
	}
	factory, err := vfs.NewFactory(roots...)
	if err != nil {
		return nil, fmt.Errorf("workspace roots: %w", err)
	}
	return factory, nil
}

// readGrantedRoots returns the durable grants, minus any the allowlist must
// not carry.
//
// A refused grant is dropped with a note on stderr rather than failing the
// launch, and this asymmetry against the flags is deliberate: a flag is typed
// for this run and a bad one should stop the operator immediately, whereas a
// grant is standing config that may have been written by an older build, under
// a different --data-dir, or against a directory since absorbed into the
// control plane. Refusing to start over one stale row would take away the
// surface an operator needs in order to run `contenox workspace remove`.
//
// `contenox workspace add` applies the same control-plane guard at grant time
// (see workspace_cmd.go); this is the second half of that promise, covering
// rows that predate the guard or were granted against another control plane.
func readGrantedRoots(cmd *cobra.Command, store runtimetypes.Store) []string {
	if store == nil {
		return nil
	}
	ctx := libtracker.WithNewRequestID(context.Background())
	granted := workspacegrants.ReadGrants(ctx, store)
	kept := make([]string, 0, len(granted))
	for _, g := range granted {
		if denied, bad := vfs.IsControlPlanePath(g); bad {
			noteSkippedGrant(cmd, g, fmt.Sprintf("it is inside the runtime's control plane (%s)", denied))
			continue
		}
		kept = append(kept, g)
	}
	return kept
}

// noteSkippedGrant tells the operator which granted root was left out of this
// run's allowlist and why, naming the verb that clears it.
func noteSkippedGrant(cmd *cobra.Command, root, why string) {
	var w io.Writer = os.Stderr
	if cmd != nil {
		w = cmd.ErrOrStderr()
	}
	fmt.Fprintf(w, "note: granted workspace root %q is not in this run's allowlist because %s — revoke it with: contenox workspace remove %s\n", root, why, root)
}
