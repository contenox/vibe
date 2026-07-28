// workspace_cmd.go is the `contenox workspace` command tree: shell-side grant
// verbs for the workspace-root allowlist that bounds a session's working
// directory. Grants are durable config in the shared database; writing one
// also publishes a bus event (workspacegrants.RootsChangedSubject), but
// nothing subscribes to it today, so there is no live reload of an
// already-open session.
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

Grants are durable config in the shared database. Writing one also publishes a
reload event on the bus, but nothing subscribes to it today, so a grant does
not apply to an already-open session — it takes effect the next time a
session is opened.

  contenox workspace add /home/me/src        # grant a root (and everything under it)
  contenox workspace add /home/me/scratch
  contenox workspace list                     # the roots you have granted
  contenox workspace remove /home/me/scratch  # revoke a grant`,
}

var workspaceAddCmd = &cobra.Command{
	Use:   "add <path>",
	Short: "Grant a directory as a project workspace root.",
	Long: `Grant <path> as a workspace root. The path must be an existing directory;
granting it grants everything under it. The grant is durable immediately, but
nothing today reloads it into an already-open session — open a new session to
pick it up. Granting a path already granted is a no-op.

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
under it) unless it is still covered by another granted root. Revoking a path
that was never granted is a no-op. The path need not still exist, so a grant
to a since-deleted directory can be cleaned up.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceRemove,
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the granted workspace roots.",
	Long: `Print the workspace roots you have granted, one per line. This is the durable
grant list these verbs manage.`,
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
	// Reject a bad friendly name before anything persists (same validation
	// project.Register applies).
	rawName, _ := cmd.Flags().GetString("name")
	name, err := project.NormalizeName(rawName)
	if err != nil {
		return fmt.Errorf("%w: %v", workspacegrants.ErrInvalidGrant, err)
	}

	// Refuse granting the runtime's own state dir (~/.contenox) as a workspace
	// root. There is no separate daemon to defer to, so this CLI computes the
	// control-plane dirs itself.
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
	// Register the granted directory as a project so it gets a friendly name
	// and a stable workspace id. The grant is already durable, so a marker
	// write failure only costs the friendly name, never the grant.
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
// No process subscribes to it today, so this is forward-looking rather than
// a working live reload; the grant itself is what's durable regardless.
// Best-effort: a publish failure is noted on stderr, never a command
// failure, since the grant is already durable.
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
		// The project's friendly name, when its marker carries one.
		if m, ok := project.ReadFromProjectRoot(r); ok && m.Name != "" {
			fmt.Fprintf(out, "  %s  (%s)\n", r, m.Name)
			continue
		}
		fmt.Fprintf(out, "  %s\n", r)
	}
}
