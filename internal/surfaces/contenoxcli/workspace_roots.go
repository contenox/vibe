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
	workspaceRootFlag = "workspace-root"

	workspaceRootsEnv = "CONTENOX_WORKSPACE_ROOTS"

	workspaceRootFlagUsage = "Directory a client may root a session in, for this run (repeatable). Adds to the launch directory and the roots granted by 'contenox workspace add'; the launch directory stays the default root. Also settable via CONTENOX_WORKSPACE_ROOTS (OS path-list separated)."
)

func addWorkspaceRootFlag(c *cobra.Command) {
	c.Flags().StringArray(workspaceRootFlag, nil, workspaceRootFlagUsage)
}

// buildWorkspaceFactory assembles the machine's workspace-root allowlist: the
// launch directory (which becomes the Factory's default root), then the durable
// grants, then --workspace-root and CONTENOX_WORKSPACE_ROOTS. The database must
// be open and BuildEngine must have registered the control-plane denylist before
// it is called.
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

// readGrantedRoots returns the durable grants, minus any the allowlist must not
// carry. A refused grant is dropped with a note on stderr rather than failing the
// launch, so a stale row cannot lock the operator out of revoking it.
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

func noteSkippedGrant(cmd *cobra.Command, root, why string) {
	var w io.Writer = os.Stderr
	if cmd != nil {
		w = cmd.ErrOrStderr()
	}
	fmt.Fprintf(w, "note: granted workspace root %q is not in this run's allowlist because %s — revoke it with: contenox workspace remove %s\n", root, why, root)
}
