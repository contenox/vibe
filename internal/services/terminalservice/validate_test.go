package terminalservice

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCwdUnderRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	sub := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, CwdUnderRoot(root, sub))
	require.Error(t, CwdUnderRoot(root, filepath.Join(t.TempDir(), "other")))
}

func TestCwdUnderRootRejectsSymlinkEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	require.Error(t, CwdUnderRoot(root, link))
}

func TestParseEnv_Disabled(t *testing.T) {
	cfg, err := ParseEnv("", "", "", "", "")
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
}

func TestParseEnv_RequiresRootWhenEnabled(t *testing.T) {
	_, err := ParseEnv("true", "", "", "", "")
	require.Error(t, err)
}

func TestParseEnv_DefaultIdleTimeout(t *testing.T) {
	root := t.TempDir()
	cfg, err := ParseEnv("true", root, "", "", "")
	require.NoError(t, err)
	require.Equal(t, DefaultIdleTimeout, cfg.IdleTimeout)
	require.Equal(t, DefaultMaxSessions, cfg.MaxSessions)
}

func TestParseEnv_CustomIdleTimeout(t *testing.T) {
	root := t.TempDir()
	cfg, err := ParseEnv("true", root, "", "5m", "")
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, cfg.IdleTimeout)
}

func TestParseEnv_DisabledIdleTimeout(t *testing.T) {
	root := t.TempDir()
	cfg, err := ParseEnv("true", root, "", "0", "")
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), cfg.IdleTimeout)
}

func TestParseEnv_InvalidIdleTimeout(t *testing.T) {
	root := t.TempDir()
	_, err := ParseEnv("true", root, "", "not-a-duration", "")
	require.Error(t, err)
}

func TestParseEnv_CustomMaxSessions(t *testing.T) {
	root := t.TempDir()
	cfg, err := ParseEnv("true", root, "", "", "4")
	require.NoError(t, err)
	require.Equal(t, 4, cfg.MaxSessions)
}

func TestParseEnv_UnlimitedMaxSessions(t *testing.T) {
	root := t.TempDir()
	cfg, err := ParseEnv("true", root, "", "", "0")
	require.NoError(t, err)
	require.Equal(t, 0, cfg.MaxSessions)
}
