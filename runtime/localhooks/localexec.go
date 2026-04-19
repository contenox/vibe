package localhooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

const localExecHookName = "local_shell"

// LocalExecResult is the structured result returned by the local_shell hook.
type LocalExecResult struct {
	ExitCode        int     `json:"exit_code"`
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
	Success         bool    `json:"success"`
	Error           string  `json:"error,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	Command         string  `json:"command,omitempty"`
}

// LocalExecHook runs commands on the local host (same machine as the process).
// It is opt-in and can be restricted by an allowlist and optional denylist. Enable via -enable-local-exec.
type LocalExecHook struct {
	defaultTimeout  time.Duration
	allowedDir      string   // if set, command path must be under this dir (after resolving)
	allowedCommands []string // if set, executable must be in this list (exact or resolved path)
	deniedCommands  []string // if set, executable basename or path must not be in this list (checked first)
}

// LocalExecOption configures LocalExecHook.
type LocalExecOption func(*LocalExecHook)

// WithLocalExecTimeout sets the default execution timeout.
func WithLocalExecTimeout(d time.Duration) LocalExecOption {
	return func(h *LocalExecHook) {
		h.defaultTimeout = d
	}
}

// WithLocalExecAllowedDir restricts execution to scripts/binaries under this directory.
func WithLocalExecAllowedDir(dir string) LocalExecOption {
	return func(h *LocalExecHook) {
		h.allowedDir = filepath.Clean(dir)
	}
}

// WithLocalExecAllowedCommands restricts execution to these executable names/paths.
func WithLocalExecAllowedCommands(commands []string) LocalExecOption {
	return func(h *LocalExecHook) {
		h.allowedCommands = commands
	}
}

// WithLocalExecDeniedCommands forbids these executable basenames or paths (checked before allowlist).
func WithLocalExecDeniedCommands(commands []string) LocalExecOption {
	return func(h *LocalExecHook) {
		h.deniedCommands = commands
	}
}

// NewLocalExecHook creates a new LocalExecHook with the given options.
func NewLocalExecHook(opts ...LocalExecOption) taskengine.HookRepo {
	h := &LocalExecHook{
		defaultTimeout: 60 * time.Second,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// resolvePolicy returns the effective allow/deny lists for this invocation.
// Chain-level context args (injected by ExecEnv via WithHookArgs) take
// precedence over the global struct-level defaults set at construction time.
// The returned map values are comma-separated lists (e.g. "git,ls").
func (h *LocalExecHook) resolvePolicy(ctx context.Context) (allowedCommands []string, allowedDir string, deniedCommands []string) {
	if args := taskengine.HookArgsFromContext(ctx, localExecHookName); len(args) > 0 {
		if v := args["_allowed_commands"]; v != "" {
			allowedCommands = splitTrimmed(v)
		}
		if v := args["_allowed_dir"]; v != "" {
			allowedDir = filepath.Clean(v)
		}
		if v := args["_denied_commands"]; v != "" {
			deniedCommands = splitTrimmed(v)
		}
		return
	}
	// Fall back to construction-time defaults.
	return h.allowedCommands, h.allowedDir, h.deniedCommands
}

// splitTrimmed splits a comma-separated string and trims whitespace.
func splitTrimmed(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Exec implements taskengine.HookRepo.
// Input is passed as stdin to the command when it is a string or when map contains "stdin".
// When invoked from execute_tool_calls, hook.Args may be nil and the command comes from input (e.g. {"command":"ls"}).
// Args (when set): command (required), args (optional space-separated), cwd, timeout, shell (default false).
func (h *LocalExecHook) Exec(ctx context.Context, startTime time.Time, input any, debug bool, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
	if hook == nil {
		return nil, taskengine.DataTypeAny, errors.New("local_shell: hook required")
	}
	if hook.Args == nil {
		hook.Args = make(map[string]string)
	}
	command, argsSlice, cwd, timeout, useShell, stdin, err := h.parseArgs(hook, input)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	allowedCommands, allowedDir, deniedCommands := h.resolvePolicy(ctx)
	if err := h.checkAllowlist(command, useShell, allowedCommands, allowedDir, deniedCommands); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	result, err := h.run(ctx, command, argsSlice, cwd, timeout, useShell, stdin)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return result, taskengine.DataTypeJSON, nil
}

func (h *LocalExecHook) parseArgs(hook *taskengine.HookCall, input any) (command string, argsSlice []string, cwd string, timeout time.Duration, useShell bool, stdin string, err error) {
	timeout = h.defaultTimeout
	// From hook.Args (string map)
	get := func(k string) string { return hook.Args[k] }
	if cmd := get("command"); cmd != "" {
		command = cmd
	}
	if a := get("args"); a != "" {
		argsSlice = strings.Fields(a)
	}
	if d := get("cwd"); d != "" {
		cwd = filepath.Clean(d)
	}
	if t := get("timeout"); t != "" {
		if d, e := time.ParseDuration(t); e == nil {
			timeout = d
		}
	}
	if s := get("shell"); s != "" {
		useShell = strings.EqualFold(s, "true") || s == "1"
	}
	// Input as stdin or as command when command not in args
	switch v := input.(type) {
	case string:
		stdin = v
		if command == "" {
			command = v
			if useShell {
				argsSlice = nil
			}
		}
	case map[string]any:
		if cmd, ok := v["command"].(string); ok && command == "" {
			command = cmd
		}
		if s, ok := v["stdin"].(string); ok {
			stdin = s
		}
		// Read shell, args, cwd, timeout from dynamic tool args if not already set by hook.Args
		if s, ok := v["shell"].(bool); ok && !useShell {
			useShell = s
		} else if s, ok := v["shell"].(string); ok && !useShell {
			useShell = strings.EqualFold(s, "true") || s == "1"
		}
		if a, ok := v["args"].(string); ok && len(argsSlice) == 0 {
			argsSlice = strings.Fields(a)
		}
		if d, ok := v["cwd"].(string); ok && cwd == "" {
			cwd = filepath.Clean(d)
		}
		if t, ok := v["timeout"].(string); ok {
			if d, e := time.ParseDuration(t); e == nil && timeout == h.defaultTimeout {
				timeout = d
			}
		}
	}
	if command == "" {
		return "", nil, "", 0, false, "", errors.New("local_shell: command is required (hook.args.command or input)")
	}
	return command, argsSlice, cwd, timeout, useShell, stdin, nil
}

func (h *LocalExecHook) checkAllowlist(command string, useShell bool, allowedCommands []string, allowedDir string, deniedCommands []string) error {
	// 🚨 Security: forbid shell mode entirely when any policy is active.
	// It is impossible to statically analyse a raw bash string for pipes (|),
	// logic operators (&&, ||) and subshells ($(...)), so we refuse to run
	// /bin/sh -c <string> when an allowlist, denylist or allowed-dir policy is
	// configured.  Without this guard, an LLM could bypass
	// This prevents the model from escaping policy via shell injection, e.g.
	// with _allowed_commands=git and: {"command":"git status; rm -rf /","shell":true}
	if useShell && (len(allowedCommands) > 0 || allowedDir != "" || len(deniedCommands) > 0) {
		return fmt.Errorf("local_shell: 'shell: true' is strictly forbidden when security " +
			"policies (allowlist / denylist / allowed-dir) are active to prevent command injection; " +
			"set shell:false and supply the command and args separately")
	}

	resolved := command
	if !filepath.IsAbs(command) {
		if path, err := exec.LookPath(command); err == nil {
			resolved = path
		} else {
			resolved = filepath.Clean(command)
		}
	} else {
		resolved = filepath.Clean(command)
	}
	// 1. Denylist: never allow these basenames or paths
	if len(deniedCommands) > 0 {
		base := filepath.Base(resolved)
		for _, d := range deniedCommands {
			dClean := filepath.Clean(d)
			if dClean == resolved || dClean == command || filepath.Base(dClean) == base || dClean == base {
				return fmt.Errorf("local_shell: command %s is denied by policy", command)
			}
		}
	}
	// 2. Sensitive default: no allow list configured = deny all
	if allowedDir == "" && len(allowedCommands) == 0 {
		return fmt.Errorf("local_shell: no allow list configured; define hook_policies in your chain JSON to allow commands or directories")
	}
	// 3. Allowlist checks
	if allowedDir != "" {
		absDir, err := filepath.Abs(allowedDir)
		if err != nil {
			return fmt.Errorf("local_shell: allowed dir invalid: %w", err)
		}
		absCmd, err := filepath.Abs(resolved)
		if err != nil {
			return fmt.Errorf("local_shell: command path invalid: %w", err)
		}
		rel, err := filepath.Rel(absDir, absCmd)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("local_shell: command %s is not under allowed dir %s", command, allowedDir)
		}
	}
	if len(allowedCommands) > 0 {
		allowed := false
		for _, c := range allowedCommands {
			cClean := filepath.Clean(c)
			if cClean == resolved || cClean == command {
				allowed = true
				break
			}
			if path, err := exec.LookPath(c); err == nil && path == resolved {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("local_shell: command %s is not in allowlist", command)
		}
	}
	return nil
}

func (h *LocalExecHook) run(ctx context.Context, command string, argsSlice []string, cwd string, timeout time.Duration, useShell bool, stdinStr string) (*LocalExecResult, error) {
	start := time.Now()
	result := &LocalExecResult{Command: command}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if useShell {
		fullCmd := command
		if len(argsSlice) > 0 {
			fullCmd += " " + strings.Join(argsSlice, " ")
		}
		cmd = exec.CommandContext(runCtx, "/bin/sh", "-c", fullCmd)
	} else {
		cmd = exec.CommandContext(runCtx, command, argsSlice...)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdinStr != "" {
		cmd.Stdin = strings.NewReader(stdinStr)
	}
	err := cmd.Run()
	result.DurationSeconds = time.Since(start).Seconds()
	result.Stdout = strings.TrimSpace(stdout.String())
	result.Stderr = strings.TrimSpace(stderr.String())
	if err != nil {
		result.Error = err.Error()
		result.Success = false
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
		return result, nil
	}
	result.ExitCode = 0
	result.Success = true
	return result, nil
}

// Supports implements taskengine.HookRegistry.
func (h *LocalExecHook) Supports(ctx context.Context) ([]string, error) {
	return []string{localExecHookName}, nil
}

// GetSchemasForSupportedHooks implements taskengine.HooksWithSchema.
func (h *LocalExecHook) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info:    &openapi3.Info{Title: "Local Exec Hook", Description: "Run commands on the local host", Version: "1.0.0"},
		Paths:   openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: map[string]*openapi3.SchemaRef{
				"LocalExecRequest": {
					Value: &openapi3.Schema{
						Type: &openapi3.Types{openapi3.TypeObject},
						Properties: map[string]*openapi3.SchemaRef{
							"command": {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "Executable path or name"}},
							"args":    {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "Space-separated arguments"}},
							"cwd":     {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "Working directory"}},
							"timeout": {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "Duration e.g. 30s"}},
							"shell":   {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeBoolean}, Description: "Run via /bin/sh -c. Set to true if you need ~ expansion, environment variables ($HOME), wildcards, pipes, or redirection. Default false."}},
						},
						Required: []string{"command"},
					},
				},
				"LocalExecResponse": {
					Value: &openapi3.Schema{
						Type: &openapi3.Types{openapi3.TypeObject},
						Properties: map[string]*openapi3.SchemaRef{
							"exit_code":        {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeInteger}}},
							"stdout":           {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}}},
							"stderr":           {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}}},
							"success":          {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeBoolean}}},
							"error":            {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}}},
							"duration_seconds": {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeNumber}}},
							"command":          {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}}},
						},
					},
				},
			},
		},
	}
	return map[string]*openapi3.T{localExecHookName: schema}, nil
}

// GetToolsForHookByName implements taskengine.HooksWithSchema.
// Chain-level policy (allowed/denied commands, allowed dir) is read from ctx
// via HookArgsFromContext when present, falling back to the struct defaults.
func (h *LocalExecHook) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != localExecHookName {
		return nil, fmt.Errorf("unknown hook: %s", name)
	}
	allowedCommands, allowedDir, deniedCommands := h.resolvePolicy(ctx)
	desc := "Run a terminal command on the local host. Input is passed as stdin."
	if len(allowedCommands) > 0 {
		desc += " Allowed commands: " + strings.Join(allowedCommands, ", ") + "."
	}
	if allowedDir != "" {
		desc += " Commands must reside under: " + allowedDir + "."
	}
	if len(deniedCommands) > 0 {
		desc += " Denied commands: " + strings.Join(deniedCommands, ", ") + "."
	}
	return []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "local_shell",
				Description: desc,
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{
							"type":        "string",
							"description": "Executable path or name (required)",
						},
						"args": map[string]interface{}{
							"type":        "string",
							"description": "Space-separated arguments",
						},
						"cwd": map[string]interface{}{
							"type":        "string",
							"description": "Working directory",
						},
						"timeout": map[string]interface{}{
							"type":        "string",
							"description": "Duration e.g. 30s",
						},
						"shell": map[string]interface{}{
							"type":        "boolean",
							"description": "Run via /bin/sh -c (default false). Set to true if you need ~ expansion, environment variables ($HOME), wildcards, pipes, or redirection. Default false.",
						},
					},
					"required": []string{"command"},
				},
			},
		},
	}, nil
}

var _ taskengine.HookRepo = (*LocalExecHook)(nil)
