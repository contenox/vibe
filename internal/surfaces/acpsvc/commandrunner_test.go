package acpsvc

import (
	"bytes"
	"context"
	"testing"

	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func TestUnit_ACPCommandRunner_RefusesWhenClientLacksTerminalCapability(t *testing.T) {
	tr := mockTransportForFS(libacp.FileSystemCapabilities{})
	runner := NewACPCommandRunnerWithShell(func(context.Context) *Transport { return tr }, localtools.DetectPlatformShell())

	var stdout, stderr bytes.Buffer
	_, err := runner.Run(context.Background(),
		localtools.CommandSpec{Command: "echo", Args: []string{"hi"}}, &stdout, &stderr)

	require.ErrorIs(t, err, localtools.ErrNoTerminal)
}

func TestUnit_ACPCommandRunner_TerminalCommandUsesDetectedShell(t *testing.T) {
	t.Parallel()
	runner := &acpCommandRunner{
		shell: localtools.NewPowerShellShell("pwsh.exe"),
	}

	command, args, title := runner.terminalCommand(localtools.CommandSpec{
		Command:  "Get-ChildItem",
		Args:     []string{"."},
		UseShell: true,
	})

	if command != "pwsh.exe" {
		t.Fatalf("expected pwsh.exe, got %q", command)
	}
	wantArgs := []string{"-NoProfile", "-NonInteractive", "-Command", "Get-ChildItem ."}
	if len(args) != len(wantArgs) {
		t.Fatalf("expected args %#v, got %#v", wantArgs, args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Fatalf("expected args %#v, got %#v", wantArgs, args)
		}
	}
	if title != "Get-ChildItem ." {
		t.Fatalf("expected title %q, got %q", "Get-ChildItem .", title)
	}
}

func TestUnit_ACPCommandRunner_TerminalCommandSpecShellOverridesRunnerShell(t *testing.T) {
	t.Parallel()
	runner := &acpCommandRunner{
		shell: localtools.NewPowerShellShell("pwsh.exe"),
	}

	command, args, _ := runner.terminalCommand(localtools.CommandSpec{
		Command:  "dir",
		UseShell: true,
		Shell:    localtools.NewCmdShell("cmd.exe"),
	})

	if command != "cmd.exe" {
		t.Fatalf("expected cmd.exe, got %q", command)
	}
	wantArgs := []string{"/D", "/C", "dir"}
	if len(args) != len(wantArgs) {
		t.Fatalf("expected args %#v, got %#v", wantArgs, args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Fatalf("expected args %#v, got %#v", wantArgs, args)
		}
	}
}
