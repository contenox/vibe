package enginebridge

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/clientfsterm"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// capsClient returns a bridgeClient serving root, without a wire connection:
// the fs and terminal methods never touch the conn, so the capability half is
// testable exactly as the agent drives it.
func capsClient(root string) *bridgeClient {
	b := &Bridge{root: root}
	if root != "" {
		b.fsterm, _ = clientfsterm.New(root)
	}
	return &bridgeClient{b: b}
}

func intPtr(v int) *int { return &v }

func TestUnit_ClientFS_ReadWriteContained(t *testing.T) {
	root := t.TempDir()
	c := capsClient(root)
	ctx := context.Background()

	_, err := c.WriteTextFile(ctx, libacp.WriteTextFileRequest{
		Path:    filepath.Join(root, "nested", "note.txt"),
		Content: "one\ntwo\nthree\nfour",
	})
	require.NoError(t, err, "a write below the root creates parents and lands")

	resp, err := c.ReadTextFile(ctx, libacp.ReadTextFileRequest{Path: filepath.Join(root, "nested", "note.txt")})
	require.NoError(t, err)
	require.Equal(t, "one\ntwo\nthree\nfour", resp.Content)

	sliced, err := c.ReadTextFile(ctx, libacp.ReadTextFileRequest{
		Path:  filepath.Join(root, "nested", "note.txt"),
		Line:  intPtr(2),
		Limit: intPtr(2),
	})
	require.NoError(t, err)
	require.Equal(t, "two\nthree", sliced.Content, "line is 1-based, limit counts lines")
}

func TestUnit_ClientFS_EscapeRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("no"), 0o644))
	c := capsClient(root)
	ctx := context.Background()

	_, err := c.ReadTextFile(ctx, libacp.ReadTextFileRequest{Path: secret})
	require.Error(t, err, "an absolute path outside the root is refused")

	_, err = c.ReadTextFile(ctx, libacp.ReadTextFileRequest{Path: filepath.Join(root, "..", filepath.Base(outside), "secret.txt")})
	require.Error(t, err, "a traversal out of the root is refused")

	_, err = c.WriteTextFile(ctx, libacp.WriteTextFileRequest{Path: secret, Content: "x"})
	require.Error(t, err, "writes are contained like reads")

	noRoot := capsClient("")
	_, err = noRoot.ReadTextFile(ctx, libacp.ReadTextFileRequest{Path: secret})
	require.ErrorIs(t, err, errNoWorkspace, "without a root the capability was never advertised")
}

func TestUnit_ClientTerminal_RunTerminalRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test drives sh")
	}
	root := t.TempDir()
	c := capsClient(root)
	ctx := context.Background()

	res, err := libacp.RunTerminal(ctx, c, libacp.CreateTerminalRequest{
		Command: "sh",
		Args:    []string{"-c", "printf out; printf err 1>&2; exit 3"},
	}, nil)
	require.NoError(t, err)
	require.Contains(t, res.Output, "out")
	require.Contains(t, res.Output, "err", "stderr shares the buffer, as the transcript shows one stream")
	require.Equal(t, 3, res.ExitCode)
	require.False(t, res.Truncated)
}

func TestUnit_ClientTerminal_TruncationKeepsTail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test drives sh")
	}
	root := t.TempDir()
	c := capsClient(root)
	limit := int64(16)

	res, err := libacp.RunTerminal(context.Background(), c, libacp.CreateTerminalRequest{
		Command:         "sh",
		Args:            []string{"-c", "printf aaaaaaaaaaaaaaaa; printf TAILTAIL"},
		OutputByteLimit: &limit,
	}, nil)
	require.NoError(t, err)
	require.True(t, res.Truncated)
	require.True(t, strings.HasSuffix(res.Output, "TAILTAIL"), "the newest bytes survive truncation, got %q", res.Output)
	require.LessOrEqual(t, len(res.Output), 16)
}

func TestUnit_ClientTerminal_CancelKillsTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test drives sh")
	}
	root := t.TempDir()
	c := capsClient(root)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := libacp.RunTerminal(ctx, c, libacp.CreateTerminalRequest{
		Command: "sh",
		Args:    []string{"-c", "sleep 30"},
	}, nil)
	require.NoError(t, err)
	require.True(t, res.TimedOut)
	require.Equal(t, -1, res.ExitCode)
	require.Less(t, time.Since(start), 10*time.Second, "the deadline must kill the process, not wait it out")
}

func TestUnit_ClientTerminal_CwdContained(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	c := capsClient(root)

	_, err := c.CreateTerminal(context.Background(), libacp.CreateTerminalRequest{
		Command: "true",
		Cwd:     outside,
	})
	require.Error(t, err, "a cwd outside the workspace root is refused")

	_, err = c.CreateTerminal(context.Background(), libacp.CreateTerminalRequest{Command: "true"})
	if runtime.GOOS != "windows" {
		require.NoError(t, err, "no cwd defaults to the workspace root")
	}
}
