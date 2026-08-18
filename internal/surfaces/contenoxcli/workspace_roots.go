package contenoxcli

import (
	"fmt"

	"github.com/contenox/contenox/internal/services/vfs"
)

// buildWorkspaceFactory builds a host's workspace containment: exactly the one
// root the host serves, fixed at launch. One instance, one workspace — a
// client that needs a different workspace attaches to a different instance.
// Editor-driven profiles build no factory at all: over ACP the client's cwd is
// authoritative. BuildEngine must have registered the control-plane denylist
// before this is called.
func buildWorkspaceFactory(root string) (*vfs.Factory, error) {
	factory, err := vfs.NewFactory(root)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	return factory, nil
}
