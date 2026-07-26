package localtools

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

type ShellKind string

const (
	ShellKindSh         ShellKind = "sh"
	ShellKindPowerShell ShellKind = "powershell"
	ShellKindCmd        ShellKind = "cmd"
)

type PlatformShell struct {
	OS          string
	Kind        ShellKind
	Command     string
	ArgsPrefix  []string
	DisplayName string
	SyntaxHint  string
}

func NewShShell(command string) PlatformShell {
	if command == "" {
		command = "/bin/sh"
	}
	return PlatformShell{
		OS:          "unix",
		Kind:        ShellKindSh,
		Command:     command,
		ArgsPrefix:  []string{"-c"},
		DisplayName: "POSIX sh",
		SyntaxHint:  "Use POSIX shell syntax: $NAME environment variables, globbing, pipes, and redirection.",
	}
}

func NewPowerShellShell(command string) PlatformShell {
	if command == "" {
		command = "powershell.exe"
	}
	return PlatformShell{
		OS:          "windows",
		Kind:        ShellKindPowerShell,
		Command:     command,
		ArgsPrefix:  []string{"-NoProfile", "-NonInteractive", "-Command"},
		DisplayName: "PowerShell",
		SyntaxHint:  "Use PowerShell syntax: $env:NAME environment variables, Windows paths, PowerShell pipelines, and PowerShell redirection rules.",
	}
}

func NewCmdShell(command string) PlatformShell {
	if command == "" {
		command = "cmd.exe"
	}
	return PlatformShell{
		OS:          "windows",
		Kind:        ShellKindCmd,
		Command:     command,
		ArgsPrefix:  []string{"/D", "/C"},
		DisplayName: "cmd.exe",
		SyntaxHint:  "Use cmd.exe syntax: %NAME% environment variables, Windows paths, cmd pipes, and cmd redirection rules.",
	}
}

func DetectPlatformShell() PlatformShell {
	return DetectPlatformShellFor(runtime.GOOS, os.Getenv, exec.LookPath)
}

func DetectPlatformShellFor(goos string, getenv func(string) string, lookPath func(string) (string, error)) PlatformShell {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if lookPath == nil {
		lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	}

	if goos == "windows" {
		for _, candidate := range []string{"pwsh.exe", "pwsh", "powershell.exe", "powershell"} {
			if path, err := lookPath(candidate); err == nil && path != "" {
				shell := NewPowerShellShell(path)
				shell.OS = goos
				return shell
			}
		}
		if comspec := strings.TrimSpace(getenv("ComSpec")); comspec != "" {
			switch shellBaseName(comspec) {
			case "pwsh", "powershell":
				shell := NewPowerShellShell(comspec)
				shell.OS = goos
				return shell
			case "cmd":
				shell := NewCmdShell(comspec)
				shell.OS = goos
				return shell
			}
		}
		if path, err := lookPath("cmd.exe"); err == nil && path != "" {
			shell := NewCmdShell(path)
			shell.OS = goos
			return shell
		}
		shell := NewCmdShell("cmd.exe")
		shell.OS = goos
		return shell
	}

	if path, err := lookPath("sh"); err == nil && path != "" {
		shell := NewShShell(path)
		shell.OS = goos
		return shell
	}
	shell := NewShShell("/bin/sh")
	shell.OS = goos
	return shell
}

func (s PlatformShell) IsSet() bool {
	return s.Command != "" && s.Kind != ""
}

func (s PlatformShell) WithDefaults() PlatformShell {
	if s.IsSet() {
		if s.OS == "" {
			s.OS = runtime.GOOS
		}
		return s
	}
	return DetectPlatformShell()
}

func (s PlatformShell) WrapCommand(command string, args []string) (program string, wrappedArgs []string, script string) {
	s = s.WithDefaults()
	script = command
	if len(args) > 0 {
		script += " " + strings.Join(args, " ")
	}
	wrappedArgs = append([]string{}, s.ArgsPrefix...)
	wrappedArgs = append(wrappedArgs, script)
	return s.Command, wrappedArgs, script
}

func (s PlatformShell) Summary() string {
	s = s.WithDefaults()
	if s.DisplayName == "" {
		return s.Command
	}
	return s.DisplayName + " (" + s.Command + ")"
}

func (s PlatformShell) ShellModeDescription() string {
	s = s.WithDefaults()
	return "Run via the detected platform shell: " + s.Summary() + ". " + s.SyntaxHint + " Set to true only when you need shell expansion, environment variables, wildcards, pipes, or redirection. Default false."
}

func shellBaseName(path string) string {
	normalized := strings.ReplaceAll(path, "\\", "/")
	normalized = strings.TrimRight(normalized, "/")
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
		normalized = normalized[idx+1:]
	}
	normalized = strings.ToLower(normalized)
	for _, suffix := range []string{".exe", ".cmd", ".bat"} {
		normalized = strings.TrimSuffix(normalized, suffix)
	}
	return normalized
}
