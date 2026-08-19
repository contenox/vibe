package echotool

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func echoExec(t *testing.T, ctx context.Context, input any) (any, taskengine.DataType, error) {
	t.Helper()
	return NewTools().Exec(ctx, time.Now(), input, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEcho})
}

// reachGrantingImports are the stdlib packages a toolset acquires filesystem,
// socket or process reach through. echo declares it has none ("reads no file,
// opens no socket, starts no process"), so its containment is by construction:
// there is no reach for vfs.Contain or the env-scrub to guard because the
// package never opens one.
var reachGrantingImports = map[string]string{
	"os":        "file, env and process reach",
	"os/exec":   "process launch",
	"os/user":   "host account reach",
	"net":       "socket reach",
	"net/http":  "network reach",
	"syscall":   "raw kernel reach",
	"io/ioutil": "file reach",
}

// TestUnit_Echo_Containment_PackageOpensNoReach is the whole containment story
// for an inert tool. escape-refused and control-plane-refused have nothing to
// resolve, and "env scrubbed where a process launches" is vacuous, because the
// package holds no reach at all: it imports none of the packages that would grant
// it. The test fails the instant echo grows such reach, forcing whoever adds it
// to route through vfs.Contain / the env-scrub rather than past them.
func TestUnit_Echo_Containment_PackageOpensNoReach(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	fset := token.NewFileSet()
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, perr := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		require.NoErrorf(t, perr, "parse %s", f)
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if why, bad := reachGrantingImports[path]; bad {
				offenders = append(offenders, fmt.Sprintf("%s imports %q (%s)", f, path, why))
			}
		}
	}
	assert.Emptyf(t, offenders,
		"echo grew reach it must not have — route it through vfs.Contain / the env-scrub, not past them:\n%s",
		strings.Join(offenders, "\n"))
}

// TestUnit_Echo_Containment_HostilePathsAreInertText proves the same boundary
// from outside: the paths vfs refuses as an escape and as a control-plane read
// are, to echo, ordinary text. echo hands them back verbatim — it never resolves,
// opens or refuses them, because it has no filesystem reach to contain. The
// vfs.Contain assertions confirm the seam local_fs uses is live and would refuse
// these very paths the instant echo tried to reach through it.
func TestUnit_Echo_Containment_HostilePathsAreInertText(t *testing.T) {
	root := t.TempDir()
	cpDir := filepath.Join(root, "control_plane")
	require.NoError(t, os.MkdirAll(cpDir, 0o755))

	// The denylist is process-global; restore whatever was registered before.
	saved := vfs.ControlPlaneDenied()
	t.Cleanup(func() { _ = vfs.SetControlPlaneDenied(saved...) })
	require.NoError(t, vfs.SetControlPlaneDenied(cpDir))

	escaping := filepath.Join(root, "..", "escaped.txt")
	cpPath := filepath.Join(cpDir, "secrets.db")

	// The seam is live: it refuses both, carrying the sentinels a caller matches on.
	_, err := vfs.Contain(root, escaping)
	require.ErrorIsf(t, err, vfs.ErrEscape, "vfs no longer refuses an escaping path; the seam is dead: %v", err)
	_, err = vfs.Contain(root, cpPath)
	require.ErrorIsf(t, err, vfs.ErrControlPlane, "vfs no longer refuses a control-plane path; the seam is dead: %v", err)

	// echo, holding no reach, treats each as opaque text: returned verbatim, as a
	// string, with no error and no resolution.
	for _, p := range []string{escaping, cpPath} {
		out, dt, execErr := echoExec(t, context.Background(), map[string]any{"input": p})
		require.NoErrorf(t, execErr, "echo refused %q; it has no fs reach to refuse with", p)
		assert.Equal(t, taskengine.DataTypeString, dt)
		assert.Equalf(t, p, out, "echo altered the hostile path %q instead of echoing it verbatim", p)
	}
}
