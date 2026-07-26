package acpsvc

import (
	"bytes"
	"context"
	"testing"

	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/libacp"
)

func TestUnit_ACPCommandRunner_FallsBackToOSWhenClientLacksTerminalCapability(t *testing.T) {
	t.Parallel()
	tr := mockTransportForFS(libacp.FileSystemCapabilities{})
	runner := NewACPCommandRunner(func() *Transport { return tr })

	var stdout, stderr bytes.Buffer
	exitCode, err := runner.Run(context.Background(),
		localtools.CommandSpec{Command: "printf", Args: []string{"hello"}},
		&stdout, &stderr)

	if err != nil {
		t.Fatalf("os fallback must run the command, got %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit 0 from os fallback, got %d", exitCode)
	}
	if stdout.String() != "hello" {
		t.Fatalf("expected os fallback stdout %q, got %q", "hello", stdout.String())
	}
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
