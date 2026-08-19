package clientfsterm

import (
	"context"
	"fmt"
	"io"

	"github.com/contenox/contenox/internal/services/localtools"
	libacp "github.com/contenox/contenox/libacp"
)

// FileIO adapts the contained server to the filesystem seam local_fs consumes,
// for a shape that owns its machine rather than proxying to a client: the run
// and mission paths, which have no ACP client to answer them. Containment and
// the control-plane refusal are the server's, so this cannot reach past the
// workspace root the caller opened.
func (s *Server) FileIO() localtools.FileIO { return fileIO{s: s} }

type fileIO struct{ s *Server }

func (f fileIO) ReadFile(ctx context.Context, path string) ([]byte, error) {
	resp, err := f.s.ReadTextFile(ctx, libacp.ReadTextFileRequest{Path: path})
	if err != nil {
		return nil, err
	}
	return []byte(resp.Content), nil
}

func (f fileIO) WriteFile(ctx context.Context, path string, data []byte) error {
	_, err := f.s.WriteTextFile(ctx, libacp.WriteTextFileRequest{Path: path, Content: string(data)})
	return err
}

// CommandRunner adapts the contained server to the seam local_shell consumes.
// The command runs through the same terminal the ACP capability serves, so it
// inherits the env scrub rather than the raw environment.
func (s *Server) CommandRunner() localtools.CommandRunner { return commandRunner{s: s} }

type commandRunner struct{ s *Server }

func (c commandRunner) Run(ctx context.Context, spec localtools.CommandSpec, stdout, stderr io.Writer) (int, error) {
	req := libacp.CreateTerminalRequest{Command: spec.Command, Args: spec.Args, Cwd: spec.Cwd}
	res, err := libacp.RunTerminal(ctx, c.s, req, nil)
	if err != nil {
		return -1, err
	}
	if stdout != nil && res.Output != "" {
		if _, werr := io.WriteString(stdout, res.Output); werr != nil {
			return res.ExitCode, fmt.Errorf("clientfsterm: relay terminal output: %w", werr)
		}
	}
	return res.ExitCode, nil
}
