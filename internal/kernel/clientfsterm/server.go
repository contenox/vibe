package clientfsterm

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/libsandbox"
	"github.com/contenox/contenox/internal/services/vfs"
	libacp "github.com/contenox/contenox/libacp"
)

// Server is the one client-side fs+terminal capability server: it answers a
// downstream agent's fs/* and terminal/* callbacks against a single workspace
// root. Reads and writes are contained by internal/services/vfs (the same seam
// local_fs uses); a launched terminal's environment is scrubbed, never the raw
// os.Environ(). Its method set satisfies agentinstance's FileSystemServer,
// TerminalServer and InstanceFileSystem without importing that package.
type Server struct {
	view *vfs.View

	// envFor scrubs a launched terminal's parent environment; never nil.
	envFor func(parent []string) []string

	terms terminals
}

// Option configures a Server.
type Option func(*Server)

// WithEnvScrub sets the filter applied to a launched terminal's parent
// environment (composed by resolvedSandboxEnv: a libsandbox scrub plus operator
// vars). A nil filter is ignored, leaving the ScrubDenySecrets default.
func WithEnvScrub(scrub func(parent []string) []string) Option {
	return func(s *Server) {
		if scrub != nil {
			s.envFor = scrub
		}
	}
}

// New returns a Server rooted at root, containment resolved through
// internal/services/vfs. An empty root is refused. Without WithEnvScrub the
// terminal environment defaults to libsandbox.ScrubDenySecrets, so a child
// process never inherits the raw parent environment.
func New(root string, opts ...Option) (*Server, error) {
	view, err := vfs.OpenView(root)
	if err != nil {
		return nil, err
	}
	s := &Server{view: view, envFor: defaultEnvScrub()}
	for _, opt := range opts {
		opt(s)
	}
	if s.envFor == nil {
		s.envFor = defaultEnvScrub()
	}
	return s, nil
}

// defaultEnvScrub is ScrubDenySecrets: the fallback that keeps a launched
// terminal off the raw os.Environ() even when no operator scrub is injected.
func defaultEnvScrub() func([]string) []string {
	scrub := libsandbox.EnvScrub(libsandbox.ScrubDenySecrets, nil, nil)
	if scrub == nil {
		return func([]string) []string { return nil }
	}
	return scrub
}

// Root returns the resolved workspace root this server serves.
func (s *Server) Root() string { return s.view.Root() }

func (s *Server) FileSystemCapabilities() libacp.FileSystemCapabilities {
	return libacp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}
}

func (s *Server) ReadTextFile(_ context.Context, req libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	path, err := s.view.Resolve(req.Path)
	if err != nil {
		return libacp.ReadTextFileResponse{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return libacp.ReadTextFileResponse{}, err
	}
	return libacp.ReadTextFileResponse{Content: sliceLines(string(data), req.Line, req.Limit)}, nil
}

func (s *Server) WriteTextFile(_ context.Context, req libacp.WriteTextFileRequest) (libacp.WriteTextFileResponse, error) {
	path, err := s.view.Resolve(req.Path)
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

// sliceLines applies the protocol's optional 1-based start line and line count
// to content; out-of-range values clamp rather than error, mirroring how an
// editor serves the same request.
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
