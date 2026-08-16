package contenoxcli

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/services/project"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/services/workspacegrants"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Grant or revoke workspace roots a session may run in.",
	Long: `Manage the durable roots in the workspace-root allowlist — the directories a
session (a terminal-UI session, a client attached to it over a pairing, or a
mission unit it dispatches) may choose as its working directory. Granting a
root grants everything UNDER it; a directory outside the allowlist is refused.

There is one allowlist, built from three sources that UNION — none overrides
or subtracts from another:

  1. the directory a surface was launched in, which is always its default root
  2. the roots granted here
  3. --workspace-root / CONTENOX_WORKSPACE_ROOTS, for that run only

So a grant never displaces the launch directory as the default, and a flag
never widens or narrows what you granted. Withdrawing access is 'workspace
remove', not remembering which flags a launcher passed.

WHEN A CHANGE TAKES EFFECT: the allowlist is read once, when a surface starts,
and a session's working directory is fixed when the session is created. A grant
added now applies to the next 'contenox new' or 'contenox resume' you START —
not to a process already running, and not to a session already open. Restart
the surface to apply it immediately.

'contenox acp' configures no allowlist at all: per the ACP protocol the editor
supplies the session's working directory.

  contenox workspace add /home/me/src        # grant a root (and everything under it)
  contenox workspace add /home/me/scratch
  contenox workspace list                     # the roots you have granted
  contenox workspace remove /home/me/scratch  # revoke a grant`,
}

var workspaceAddCmd = &cobra.Command{
	Use:   "add <path>",
	Short: "Grant a directory as a project workspace root.",
	Long: `Grant <path> as a workspace root. The path must be an existing directory;
granting it grants everything under it. The grant is durable immediately, but a
surface already running read its allowlist when it started — start a new
'contenox new' or 'contenox resume' to pick this up. Granting a path already
granted is a no-op.

The granted directory is also registered as a project: its
.contenox/workspace.id marker is created if absent, and --name stamps a friendly
display name into it (shown by 'workspace list', the API, and the beam picker;
default: the folder's basename shows). Re-adding an already-granted path with a
new --name renames the project.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceAdd,
}

var workspaceRemoveCmd = &cobra.Command{
	Use:   "remove <path>",
	Short: "Revoke a workspace-root grant.",
	Long: `Revoke the grant for <path>. Sessions started after this may no longer choose
it (or anything under it) unless it is still covered by another granted root,
by a surface's launch directory, or by a --workspace-root passed to that run.
Revoking does not evict a session already running there, and a surface already
running keeps the allowlist it started with — restart it to apply the change.
Revoking a path that was never granted is a no-op. The path need not still
exist, so a grant to a since-deleted directory can be cleaned up.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceRemove,
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the granted workspace roots.",
	Long: `Print the workspace roots you have granted, one per line. This is the durable
grant list these verbs manage — the granted source only. A running surface's
full allowlist also includes the directory it was launched in and any
--workspace-root passed to it, which it advertises to its own client.`,
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
	// Reject a bad friendly name before anything persists.
	rawName, _ := cmd.Flags().GetString("name")
	name, err := project.NormalizeName(rawName)
	if err != nil {
		return fmt.Errorf("%w: %v", workspacegrants.ErrInvalidGrant, err)
	}

	// Refuse granting the runtime's own state dir as a workspace root.
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
	// The grant is already durable, so a marker write failure only costs the
	// friendly name.
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

// ringReloadDoorbell publishes the workspace-roots-changed event on the bus.
// Nothing in-process subscribes: a session's cwd is fixed at session/new, so a
// live reload could only widen what a future session may pick. Best-effort.
func ringReloadDoorbell(ctx context.Context, cmd *cobra.Command, db libdb.DBManager, roots []string) {
	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	if err := workspacegrants.PublishChanged(ctx, bus, roots); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: workspace-root change saved, but publishing the reload event failed: %v\n", err)
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

		if m, ok := project.ReadFromProjectRoot(r); ok && m.Name != "" {
			fmt.Fprintf(out, "  %s  (%s)\n", r, m.Name)
			continue
		}
		fmt.Fprintf(out, "  %s\n", r)
	}
}
