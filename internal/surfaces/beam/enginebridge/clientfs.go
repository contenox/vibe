package enginebridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/surfaces/beam/vfs"
	libacp "github.com/contenox/contenox/libacp"
)

// errNoWorkspace answers an fs or terminal call arriving without a workspace
// root. Initialize advertised neither capability then, so reaching here means
// the agent ignored the advertisement.
var errNoWorkspace = errors.New("enginebridge: no workspace root; fs and terminal were not advertised")

// containPath resolves an agent-supplied path inside the bridge's root; the
// agent is on the other side of a protocol boundary, so its paths are inputs,
// not facts.
func (b *Bridge) containPath(path string) (string, error) {
	if b.root == "" {
		return "", errNoWorkspace
	}
	resolved, err := vfs.Contain(b.root, path)
	if err != nil {
		return "", fmt.Errorf("enginebridge: %w", err)
	}
	return resolved, nil
}

func (c *bridgeClient) ReadTextFile(_ context.Context, req libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	path, err := c.b.containPath(req.Path)
	if err != nil {
		return libacp.ReadTextFileResponse{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return libacp.ReadTextFileResponse{}, err
	}
	return libacp.ReadTextFileResponse{Content: sliceLines(string(data), req.Line, req.Limit)}, nil
}

func (c *bridgeClient) WriteTextFile(_ context.Context, req libacp.WriteTextFileRequest) (libacp.WriteTextFileResponse, error) {
	path, err := c.b.containPath(req.Path)
	if err != nil {
		return libacp.WriteTextFileResponse{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return libacp.WriteTextFileResponse{}, err
	}
	if err := os.WriteFile(path, []byte(req.Content), 0o644); err != nil {
		return libacp.WriteTextFileResponse{}, err
	}
	return libacp.WriteTextFileResponse{}, nil
}

// sliceLines applies the protocol's optional 1-based start line and line
// count to content; out-of-range values clamp rather than error, mirroring
// how editors serve the same request.
func sliceLines(content string, line, limit *int) string {
	if line == nil && limit == nil {
		return content
	}
	lines := strings.Split(content, "\n")
	start := 0
	if line != nil && *line > 1 {
		start = *line - 1
	}
	if start >= len(lines) {
		return ""
	}
	end := len(lines)
	if limit != nil && *limit >= 0 && start+*limit < end {
		end = start + *limit
	}
	return strings.Join(lines[start:end], "\n")
}
