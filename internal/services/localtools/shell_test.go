package localtools_test

import (
	"errors"
	"testing"

	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/assert"
)

func TestUnit_PlatformShell_DetectWindowsPrefersPwsh(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name == "pwsh.exe" {
			return `C:\Program Files\PowerShell\7\pwsh.exe`, nil
		}
		return "", errors.New("missing")
	}

	shell := localtools.DetectPlatformShellFor("windows", nil, lookPath)

	assert.Equal(t, localtools.ShellKindPowerShell, shell.Kind)
	assert.Equal(t, "windows", shell.OS)
	assert.Equal(t, `C:\Program Files\PowerShell\7\pwsh.exe`, shell.Command)
	program, args, script := shell.WrapCommand("Get-ChildItem", []string{"."})
	assert.Equal(t, shell.Command, program)
	assert.Equal(t, "Get-ChildItem .", script)
	assert.Equal(t, []string{"-NoProfile", "-NonInteractive", "-Command", "Get-ChildItem ."}, args)
}

func TestUnit_PlatformShell_DetectWindowsFallsBackToComSpecCmd(t *testing.T) {
	getenv := func(key string) string {
		if key == "ComSpec" {
			return `C:\Windows\System32\cmd.exe`
		}
		return ""
	}
	lookPath := func(string) (string, error) {
		return "", errors.New("missing")
	}

	shell := localtools.DetectPlatformShellFor("windows", getenv, lookPath)

	assert.Equal(t, localtools.ShellKindCmd, shell.Kind)
	assert.Equal(t, `C:\Windows\System32\cmd.exe`, shell.Command)
	assert.Equal(t, []string{"/D", "/C"}, shell.ArgsPrefix)
}

func TestUnit_PlatformShell_DetectUnixUsesSh(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name == "sh" {
			return "/usr/bin/sh", nil
		}
		return "", errors.New("missing")
	}

	shell := localtools.DetectPlatformShellFor("linux", nil, lookPath)

	assert.Equal(t, localtools.ShellKindSh, shell.Kind)
	assert.Equal(t, "linux", shell.OS)
	assert.Equal(t, "/usr/bin/sh", shell.Command)
	assert.Equal(t, []string{"-c"}, shell.ArgsPrefix)
}
