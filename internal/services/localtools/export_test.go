package localtools

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/libdbexec"
)

// ExportedIsBlockedEgressIP is a test-only export of the link-local / cloud-metadata predicate.
func ExportedIsBlockedEgressIP(ip net.IP) bool { return isBlockedEgressIP(ip) }

type TestHostFileIO struct{}

func (TestHostFileIO) ReadFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (TestHostFileIO) WriteFile(_ context.Context, path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func NewLocalFSToolsForTest(allowedDir string, db libdb.DBManager, opts ...FSOption) taskengine.ToolsRepo {
	return NewLocalFSToolsWith(allowedDir, db, TestHostFileIO{}, LocalFSToolsName, nil, opts...)
}

func NewTestHostRunner() CommandRunner {
	return TestHostCommandRunner{shell: DetectPlatformShell().WithDefaults()}
}

type TestHostCommandRunner struct {
	shell PlatformShell
}

func (r TestHostCommandRunner) Run(ctx context.Context, spec CommandSpec, stdout, stderr io.Writer) (int, error) {
	var cmd *exec.Cmd
	if spec.UseShell {
		shell := spec.Shell
		if !shell.IsSet() {
			shell = r.shell
		}
		program, args, _ := shell.WrapCommand(spec.Command, spec.Args)
		cmd = exec.CommandContext(ctx, program, args...)
	} else {
		cmd = exec.CommandContext(ctx, spec.Command, spec.Args...)
	}
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	// cmd.Env nil means "inherit os.Environ()", handing the child every secret in serve's environment; the scrub replaces it to close that leak.
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), err
		}
		return -1, err
	}
	return 0, nil
}
