package clientfsterm_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/kernel/clientfsterm"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// The shared server is the concrete backing for agentinstance's client-side
// capability interfaces; drift in either method set breaks this at compile time.
var (
	_ agentinstance.FileSystemServer   = (*clientfsterm.Server)(nil)
	_ agentinstance.TerminalServer     = (*clientfsterm.Server)(nil)
	_ agentinstance.InstanceFileSystem = (*clientfsterm.Server)(nil)
)

func newServer(t *testing.T, root string, opts ...clientfsterm.Option) *clientfsterm.Server {
	t.Helper()
	s, err := clientfsterm.New(root, opts...)
	require.NoError(t, err)
	return s
}

func intPtr(v int) *int { return &v }

func TestUnit_EmptyRootRefused(t *testing.T) {
	_, err := clientfsterm.New("")
	require.Error(t, err, "a server without a workspace root is refused")
}

func TestUnit_FS_ReadWriteContained(t *testing.T) {
	root := t.TempDir()
	s := newServer(t, root)
	ctx := context.Background()

	_, err := s.WriteTextFile(ctx, libacp.WriteTextFileRequest{
		Path:    filepath.Join(root, "nested", "note.txt"),
		Content: "one\ntwo\nthree\nfour",
	})
	require.NoError(t, err, "a write below the root creates parents and lands")

	resp, err := s.ReadTextFile(ctx, libacp.ReadTextFileRequest{Path: filepath.Join(root, "nested", "note.txt")})
	require.NoError(t, err)
	require.Equal(t, "one\ntwo\nthree\nfour", resp.Content)

	sliced, err := s.ReadTextFile(ctx, libacp.ReadTextFileRequest{
		Path:  filepath.Join(root, "nested", "note.txt"),
		Line:  intPtr(2),
		Limit: intPtr(2),
	})
	require.NoError(t, err)
	require.Equal(t, "two\nthree", sliced.Content, "line is 1-based, limit counts lines")
}

func TestUnit_FS_EscapeRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("no"), 0o644))
	s := newServer(t, root)
	ctx := context.Background()

	_, err := s.ReadTextFile(ctx, libacp.ReadTextFileRequest{Path: secret})
	require.Error(t, err, "an absolute path outside the root is refused")

	_, err = s.ReadTextFile(ctx, libacp.ReadTextFileRequest{Path: filepath.Join(root, "..", filepath.Base(outside), "secret.txt")})
	require.Error(t, err, "a traversal out of the root is refused")

	_, err = s.WriteTextFile(ctx, libacp.WriteTextFileRequest{Path: secret, Content: "x"})
	require.Error(t, err, "writes are contained like reads")
}

func TestUnit_Terminal_RunRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test drives sh")
	}
	root := t.TempDir()
	s := newServer(t, root)

	res, err := libacp.RunTerminal(context.Background(), s, libacp.CreateTerminalRequest{
		Command: "sh",
		Args:    []string{"-c", "printf out; printf err 1>&2; exit 3"},
	}, nil)
	require.NoError(t, err)
	require.Contains(t, res.Output, "out")
	require.Contains(t, res.Output, "err", "stderr shares the buffer, as the transcript shows one stream")
	require.Equal(t, 3, res.ExitCode)
	require.False(t, res.Truncated)
}

func TestUnit_Terminal_TruncationKeepsTail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test drives sh")
	}
	root := t.TempDir()
	s := newServer(t, root)
	limit := int64(16)

	res, err := libacp.RunTerminal(context.Background(), s, libacp.CreateTerminalRequest{
		Command:         "sh",
		Args:            []string{"-c", "printf aaaaaaaaaaaaaaaa; printf TAILTAIL"},
		OutputByteLimit: &limit,
	}, nil)
	require.NoError(t, err)
	require.True(t, res.Truncated)
	require.True(t, strings.HasSuffix(res.Output, "TAILTAIL"), "the newest bytes survive truncation, got %q", res.Output)
	require.LessOrEqual(t, len(res.Output), 16)
}

func TestUnit_Terminal_CancelKillsTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test drives sh")
	}
	root := t.TempDir()
	s := newServer(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := libacp.RunTerminal(ctx, s, libacp.CreateTerminalRequest{
		Command: "sh",
		Args:    []string{"-c", "sleep 30"},
	}, nil)
	require.NoError(t, err)
	require.True(t, res.TimedOut)
	require.Equal(t, -1, res.ExitCode)
	require.Less(t, time.Since(start), 10*time.Second, "the deadline must kill the process, not wait it out")
}

func TestUnit_Terminal_CwdContained(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	s := newServer(t, root)

	_, err := s.CreateTerminal(context.Background(), libacp.CreateTerminalRequest{
		Command: "true",
		Cwd:     outside,
	})
	require.Error(t, err, "a cwd outside the workspace root is refused")

	_, err = s.CreateTerminal(context.Background(), libacp.CreateTerminalRequest{Command: "true"})
	if runtime.GOOS != "windows" {
		require.NoError(t, err, "no cwd defaults to the workspace root")
	}
}

// TestUnit_Terminal_EnvNeverRawEnviron is the regression guard: the launched
// child must not inherit the raw parent environment. Under the default scrub a
// secret-shaped parent var is stripped, and an injected scrub is honored
// verbatim.
func TestUnit_Terminal_EnvNeverRawEnviron(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test drives sh")
	}
	t.Setenv("FSTERM_TEST_TOKEN", "leak-me")
	t.Setenv("FSTERM_TEST_PLAIN", "kept")

	root := t.TempDir()

	// Default posture (ScrubDenySecrets): *_TOKEN is denied, a plain var passes.
	def := newServer(t, root)
	res, err := libacp.RunTerminal(context.Background(), def, libacp.CreateTerminalRequest{
		Command: "sh",
		Args:    []string{"-c", `printf 'token=[%s] plain=[%s]' "$FSTERM_TEST_TOKEN" "$FSTERM_TEST_PLAIN"`},
	}, nil)
	require.NoError(t, err)
	require.Contains(t, res.Output, "token=[]", "a secret-shaped parent var is scrubbed, not inherited raw")
	require.Contains(t, res.Output, "plain=[kept]", "a plain parent var survives the default scrub")

	// An injected scrub is the authority over the child's environment.
	injected := newServer(t, root, clientfsterm.WithEnvScrub(func([]string) []string {
		return []string{"FSTERM_ONLY=1"}
	}))
	res, err = libacp.RunTerminal(context.Background(), injected, libacp.CreateTerminalRequest{
		Command: "sh",
		Args:    []string{"-c", `printf 'only=[%s] token=[%s]' "$FSTERM_ONLY" "$FSTERM_TEST_TOKEN"`},
	}, nil)
	require.NoError(t, err)
	require.Contains(t, res.Output, "only=[1]", "the injected scrub's vars reach the child")
	require.Contains(t, res.Output, "token=[]", "the injected scrub replaced the parent environment")
}

// TestUnit_Terminal_RequestEnvLayersOnScrub proves the agent's requested vars
// are appended after the scrub, so they win.
func TestUnit_Terminal_RequestEnvLayersOnScrub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test drives sh")
	}
	root := t.TempDir()
	s := newServer(t, root)

	res, err := libacp.RunTerminal(context.Background(), s, libacp.CreateTerminalRequest{
		Command: "sh",
		Args:    []string{"-c", `printf '[%s]' "$FSTERM_REQ"`},
		Env:     []libacp.EnvVariable{{Name: "FSTERM_REQ", Value: "here"}},
	}, nil)
	require.NoError(t, err)
	require.Contains(t, res.Output, "[here]", "the request's env vars are layered onto the scrubbed environment")
}
