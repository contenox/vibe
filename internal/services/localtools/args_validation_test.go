package localtools_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

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
