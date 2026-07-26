//go:build !windows

package terminalservice

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateShell(t *testing.T) {
	require.NoError(t, ValidateShell("/bin/bash"))
	require.Error(t, ValidateShell("bash"))
	require.Error(t, ValidateShell("/usr/bin/evil"))
}
