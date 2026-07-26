//go:build windows

package terminalservice

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateShell(t *testing.T) {
	require.NoError(t, ValidateShell(`C:\Windows\System32\cmd.exe`))
	require.NoError(t, ValidateShell("powershell.exe"))
	require.Error(t, ValidateShell("bash"))
	require.Error(t, ValidateShell(`C:\evil\shell.exe`))
}
