// workspace_cmd.go is the `contenox workspace` command tree — the shell-side
// grant verbs for the workspace-root allowlist that bounds what a session (a
// chat, a dispatched unit, a Beam file browse) may choose as its working
// directory.
//
// Unlike `contenox fleet` / `mission`, these do NOT reach serve over REST. A
// grant is DURABLE config in the shared ~/.contenox/local.db every process opens,
// so `workspace add/remove` writes it directly (the config-command register of
// `contenox config`) and then rings a reload doorbell on the shared SQLite bus —
// the same cross-process rail the report-routing slice proved (CLI writer → serve
// reader). A running `contenox serve` re-reads the config on that signal and
// swaps its live root set without a restart; a serve started later reads the same
// durable config at boot. So these verbs work whether or not serve is up, and a
// running serve applies them live.
//
// `workspace` manages the GRANTS — the roots an operator adds beyond serve's
// launch-time roots (its served directory, --workspace-root flags,
// WORKSPACE_ROOTS). serve always also allows its own launched default root; that
// one is not a grant and is not listed here (it shows in the API / Beam picker,
// GET /workspace/roots).
package contenoxcli

import (
	"context"
	"fmt"

	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/project"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/services/workspacegrants"
	"github.com/spf13/cobra"
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Grant or revoke workspace roots a session may run in.",
	Long: `Manage the workspace-root allowlist — the directories a session (a chat, a
dispatched mission unit, or a Beam file browse) may choose as its working
directory. Granting a root grants everything UNDER it; a directory outside every
granted root is refused.

Grants are durable config in the shared database, so these verbs work whether or
not 'contenox serve' is running. When serve IS running, a grant applies LIVE —
these verbs ring a reload signal serve picks up without a restart.

  contenox workspace add /home/me/src        # grant a root (and everything under it)
  contenox workspace add /home/me/scratch
  contenox workspace list                     # the roots you have granted
  contenox workspace remove /home/me/scratch  # revoke a grant

serve always also allows its own launched default root (its served directory, or
home for a bare 'contenox serve'); that is not a grant and is not listed here —
it appears in the API and the Beam folder picker (GET /workspace/roots).`,
}

var workspaceAddCmd = &cobra.Command{
	Use:   "add <path>",
	Short: "Grant a directory as a project workspace root.",
	Long: `Grant <path> as a workspace root. The path must be an existing directory;
granting it grants everything under it. The grant is durable and, if a
'contenox serve' is running, applies live (no restart). Granting a path already
granted is a no-op.

The granted directory is also registered as a project: its
.contenox/workspace.id marker is created if absent, and --name stamps a friendly
display name into it (shown by 'workspace list', the API, and the Beam picker;
default: the folder's basename shows). Re-adding an already-granted path with a
new --name renames the project.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceAdd,
}

var workspaceRemoveCmd = &cobra.Command{
	Use:   "remove <path>",
	Short: "Revoke a workspace-root grant.",
	Long: `Revoke the grant for <path>. Sessions may no longer choose it (or anything
under it) unless it is still covered by another granted root or serve's launched
default. Revoking a path that was never granted is a no-op. The path need not
still exist, so a grant to a since-deleted directory can be cleaned up.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceRemove,
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the granted workspace roots.",
	Long: `Print the workspace roots you have granted, one per line. This is the durable
grant list these verbs manage; serve additionally allows its own launched default
root, which is shown by the API and the Beam folder picker, not here.`,
	Args: cobra.NoArgs,
	RunE: runWorkspaceList,
}

func init() {
	workspaceAddCmd.Flags().String("name", "", "Friendly project name stamped into the folder's marker (default: keep an existing name, else the folder name shows)")
	workspaceCmd.AddCommand(workspaceAddCmd)
	workspaceCmd.AddCommand(workspaceRemoveCmd)
	workspaceCmd.AddCommand(workspaceListCmd)
}

func runWorkspaceAdd(cmd *cobra.Command, args []string) error {
	// Same boundary rule as the REST mutator (workspace_reload.go): a bad friendly
	// name is refused BEFORE anything persists, never silently dropped.
	rawName, _ := cmd.Flags().GetString("name")
	name, err := project.NormalizeName(rawName)
	if err != nil {
		return fmt.Errorf("%w: %v", workspacegrants.ErrInvalidGrant, err)
	}

	// Control-plane isolation (vfs-invariant slice): refuse granting the runtime's
	// own state dir (~/.contenox: config, database, HITL policies, declared agents)
	// as a workspace root. This CLI runs in a SEPARATE process from serve, which is
	// where the vfs global denylist is set, so we compute the control-plane dirs
	// here (the same set serve denies) and check against them directly. See
	// runtime/vfs/controlplane.go.
	if contenoxDir, derr := ResolveContenoxDir(cmd); derr == nil {
		if denied, ok := vfs.WithinControlPlane(controlPlaneDirs(contenoxDir), args[0]); ok {
			return fmt.Errorf("%w: %q is inside the runtime's control plane (%s) and can never be a workspace root — the runtime never lets a session reach its own config, database, or policies", workspacegrants.ErrInvalidGrant, args[0], denied)
		}
	}

	db, store, err := openConfigDB(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := libtracker.WithNewRequestID(context.Background())
	roots, err := workspacegrants.Add(ctx, store, args[0])
	if err != nil {
		return err
	}
	// Register the granted directory as a project — the SAME best-effort marker
	// stamp the serve-side REST mutator applies (fresh markers default their name
	// to the folder basename; an explicit --name renames), so a CLI grant and a
	// Beam "Add project" produce the same on-disk result. The grant is already
	// durable, so a marker write failure (e.g. a read-only folder) only costs the
	// friendly name, never the grant.
	if _, merr := project.Register(args[0], name); merr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: root granted, but its project marker could not be written (the name falls back to the folder name): %v\n", merr)
	}
	ringReloadDoorbell(ctx, cmd, db, roots)
	printWorkspaceGrants(cmd, roots)
	return nil
}

func runWorkspaceRemove(cmd *cobra.Command, args []string) error {
	db, store, err := openConfigDB(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := libtracker.WithNewRequestID(context.Background())
	roots, err := workspacegrants.Remove(ctx, store, args[0])
	if err != nil {
		return err
	}
	ringReloadDoorbell(ctx, cmd, db, roots)
	printWorkspaceGrants(cmd, roots)
	return nil
}

func runWorkspaceList(cmd *cobra.Command, args []string) error {
	db, store, err := openConfigDB(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := libtracker.WithNewRequestID(context.Background())
	printWorkspaceGrants(cmd, workspacegrants.ReadGrants(ctx, store))
	return nil
}

// ringReloadDoorbell publishes the reload signal on the shared SQLite bus so a
// running serve applies the change live. Best-effort by contract: the grant is
// already durable, so a publish failure (no serve to hear it, a bus hiccup)
// leaves the grant intact and serve converges on its next boot or signal — it is
// noted on stderr, never a command failure.
func ringReloadDoorbell(ctx context.Context, cmd *cobra.Command, db libdb.DBManager, roots []string) {
	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	if err := workspacegrants.PublishChanged(ctx, bus, roots); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: workspace-root change saved, but the live-reload signal to a running serve failed: %v\n", err)
	}
}

func printWorkspaceGrants(cmd *cobra.Command, roots []string) {
	out := cmd.OutOrStdout()
	if len(roots) == 0 {
		fmt.Fprintln(out, "(no workspace-root grants configured)")
		return
	}
	fmt.Fprintln(out, "Granted workspace roots:")
	for _, r := range roots {
		// The project's friendly name, when its marker carries one — the same label
		// the API and the Beam picker show for this root.
		if m, ok := project.ReadFromProjectRoot(r); ok && m.Name != "" {
			fmt.Fprintf(out, "  %s  (%s)\n", r, m.Name)
			continue
		}
		fmt.Fprintf(out, "  %s\n", r)
	}
}
