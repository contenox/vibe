package terminalservice

import (
	"os"
	"path/filepath"
	"testing"

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
