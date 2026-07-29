package localtools_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func TestUnit_EchoTools_RejectsUnknownArgs(t *testing.T) {
	tools := localtools.NewEchoTools()

	_, _, err := tools.Exec(context.Background(), time.Now().UTC(), map[string]any{
		"input":      "hello",
		"unexpected": true,
	}, false, &taskengine.ToolsCall{Name: "echo"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown argument")
	require.Contains(t, err.Error(), "unexpected")
}

func TestUnit_PrintTools_RejectsUnknownArgs(t *testing.T) {
	tools := localtools.NewPrint(libtracker.NoopTracker{})

	_, _, err := tools.Exec(context.Background(), time.Now().UTC(), map[string]any{
		"message":    "hello",
		"unexpected": true,
	}, false, &taskengine.ToolsCall{Name: "print"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown argument")
	require.Contains(t, err.Error(), "unexpected")
}

func TestUnit_SSHTools_RejectsUnknownArgsBeforeDial(t *testing.T) {
	tools, err := localtools.NewSSHTools(localtools.WithCustomHostKeyCallback(ssh.InsecureIgnoreHostKey()))
	require.NoError(t, err)

	_, _, err = tools.Exec(context.Background(), time.Now().UTC(), map[string]any{
		"host":       "127.0.0.1",
		"user":       "nobody",
		"command":    "true",
		"unexpected": true,
	}, false, &taskengine.ToolsCall{Name: "ssh"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown argument")
	require.Contains(t, err.Error(), "unexpected")
}
