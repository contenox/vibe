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

func TestValidateShell(t *testing.T) {
	require.NoError(t, ValidateShell("/bin/bash"))
	require.Error(t, ValidateShell("bash"))
	require.Error(t, ValidateShell("/usr/bin/evil"))
}

func TestParseEnv_Disabled(t *testing.T) {
	cfg, err := ParseEnv("", "", "", "")
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
}

func TestParseEnv_RequiresRootWhenEnabled(t *testing.T) {
	_, err := ParseEnv("true", "", "", "")
	require.Error(t, err)
}

func TestParseEnv_DefaultIdleTimeout(t *testing.T) {
	root := t.TempDir()
	cfg, err := ParseEnv("true", root, "", "")
	require.NoError(t, err)
	require.Equal(t, DefaultIdleTimeout, cfg.IdleTimeout)
}

func TestParseEnv_CustomIdleTimeout(t *testing.T) {
	root := t.TempDir()
	cfg, err := ParseEnv("true", root, "", "5m")
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, cfg.IdleTimeout)
}

func TestParseEnv_DisabledIdleTimeout(t *testing.T) {
	root := t.TempDir()
	cfg, err := ParseEnv("true", root, "", "0")
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), cfg.IdleTimeout)
}

func TestParseEnv_InvalidIdleTimeout(t *testing.T) {
	root := t.TempDir()
	_, err := ParseEnv("true", root, "", "not-a-duration")
	require.Error(t, err)
}
