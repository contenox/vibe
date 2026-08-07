package localtools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

const localExecToolsName = "local_shell"

// LocalExecResult is the structured result returned by the local_shell tools.
type LocalExecResult struct {
	ExitCode        int     `json:"exit_code"`
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
	Success         bool    `json:"success"`
	Error           string  `json:"error,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	Command         string  `json:"command,omitempty"`
	Shell           string  `json:"shell,omitempty"`
	OS              string  `json:"os,omitempty"`
}

// LocalExecTools runs commands on the local host (same machine as the process).
// It is opt-in and can be restricted by an allowlist and optional denylist. Enable via -enable-local-exec.
type LocalExecTools struct {
	defaultTimeout  time.Duration
	allowedDir      string   // if set, command path must be under this dir (after resolving)
	allowedCommands []string // if set, executable must be in this list (exact or resolved path)
	deniedCommands  []string // if set, executable basename or path must not be in this list (checked first)
	runner          CommandRunner
	shell           PlatformShell
	scrubEnv        func([]string) []string // if set, the default runner scrubs the child environment
}

// LocalExecOption configures LocalExecTools.
type LocalExecOption func(*LocalExecTools)

// WithLocalExecTimeout sets the default execution timeout.
func WithLocalExecTimeout(d time.Duration) LocalExecOption {
	return func(h *LocalExecTools) {
		h.defaultTimeout = d
	}
}

// WithLocalExecAllowedDir restricts execution to scripts/binaries under this directory.
func WithLocalExecAllowedDir(dir string) LocalExecOption {
	return func(h *LocalExecTools) {
		h.allowedDir = filepath.Clean(dir)
	}
}

// WithLocalExecAllowedCommands restricts execution to these executable names/paths.
func WithLocalExecAllowedCommands(commands []string) LocalExecOption {
	return func(h *LocalExecTools) {
		h.allowedCommands = commands
	}
}

// WithLocalExecDeniedCommands forbids these executable basenames or paths (checked before allowlist).
func WithLocalExecDeniedCommands(commands []string) LocalExecOption {
	return func(h *LocalExecTools) {
		h.deniedCommands = commands
	}
}

// WithLocalExecShell sets the detected platform shell used for shell:true calls
// and for tool schema descriptions.
func WithLocalExecShell(shell PlatformShell) LocalExecOption {
	return func(h *LocalExecTools) {
		h.shell = shell.WithDefaults()
	}
}

// WithLocalExecScrubEnv sets the environment scrub applied to every local_shell
// command run through the default runner: `scrub` maps the parent environment to
// the one the command inherits, so credentials in serve's environment do not leak
// into an LLM-driven shell (see libsandbox.EnvScrub). Nil (the default) keeps the
// full inherited environment. Ignored when an explicit runner is supplied to
// NewLocalExecToolsWith, which owns its own environment handling.
func WithLocalExecScrubEnv(scrub func([]string) []string) LocalExecOption {
	return func(h *LocalExecTools) {
		h.scrubEnv = scrub
	}
}

// NewLocalExecTools creates a new LocalExecTools with the given options.
func NewLocalExecTools(opts ...LocalExecOption) taskengine.ToolsRepo {
	return NewLocalExecToolsWith(nil, opts...)
}

func NewLocalExecToolsWith(runner CommandRunner, opts ...LocalExecOption) taskengine.ToolsRepo {
	h := &LocalExecTools{
		// The agent is told to verify its work, and verification is what takes
		// time here: `go build ./...`, a test suite, `npm ci`. At 60s those were
		// killed mid-run and came back as tool failures the model then tried to
		// work around, spending tool rounds on a command that was never wrong.
		// Per-call `timeout` still narrows this for a command that should be
		// quick.
		defaultTimeout: 10 * time.Minute,
		shell:          DetectPlatformShell(),
	}
	for _, opt := range opts {
		opt(h)
	}
	if runner == nil {
		runner = NewOSCommandRunnerWithShellAndScrub(h.shell, h.scrubEnv)
	}
	h.runner = runner
	return h
}

// resolvePolicy returns the effective allow/deny lists for this invocation.
// Chain-level context args (injected by ExecEnv via WithToolsArgs) take
// precedence over the global struct-level defaults set at construction time.
// The returned map values are comma-separated lists (e.g. "git,ls").
func (h *LocalExecTools) resolvePolicy(ctx context.Context) (allowedCommands []string, allowedDir string, deniedCommands []string) {
	if args := taskengine.ToolsArgsFromContext(ctx, localExecToolsName); len(args) > 0 {
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

// Exec implements taskengine.ToolsRepo.
// Input is passed as stdin to the command when it is a string or when map contains "stdin".
// When invoked from execute_tool_calls, tools.Args may be nil and the command comes from input (e.g. {"command":"ls"}).
// Args (when set): command (required), args (optional space-separated), cwd, timeout, shell (default false).
func (h *LocalExecTools) Exec(ctx context.Context, startTime time.Time, input any, debug bool, tools *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if tools == nil {
		return nil, taskengine.DataTypeAny, errors.New("local_shell: tools required")
	}
	if tools.Args == nil {
		tools.Args = make(map[string]string)
	}
	command, argsSlice, cwd, timeout, useShell, stdin, err := h.parseArgs(tools, input)
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

func (h *LocalExecTools) parseArgs(tools *taskengine.ToolsCall, input any) (command string, argsSlice []string, cwd string, timeout time.Duration, useShell bool, stdin string, err error) {
	timeout = h.defaultTimeout
	// From tools.Args (string map)
	get := func(k string) string { return tools.Args[k] }
	if cmd := get("command"); cmd != "" {
		command = cmd
	}
	if a := get("args"); a != "" {
		argsSlice = splitShellArgs(a)
	}
	if d := get("cwd"); d != "" {
		cwd = filepath.Clean(d)
		if absCwd, err := filepath.Abs(cwd); err == nil {
			cwd = absCwd
		}
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
		if err := rejectUnknownArgs(localExecToolsName, v, "command", "args", "cwd", "timeout", "shell", "stdin"); err != nil {
			return "", nil, "", 0, false, "", err
		}
		if cmd, ok := v["command"].(string); ok && command == "" {
			command = cmd
		}
		if s, ok := v["stdin"].(string); ok {
			stdin = s
		}
		// Read shell, args, cwd, timeout from dynamic tool args if not already set by tools.Args
		if s, ok := v["shell"].(bool); ok && !useShell {
			useShell = s
		} else if s, ok := v["shell"].(string); ok && !useShell {
			useShell = strings.EqualFold(s, "true") || s == "1"
		}
		if a, ok := v["args"]; ok && len(argsSlice) == 0 {
			parsed, err := stringSliceArg(localExecToolsName, "args", a)
			if err != nil {
				return "", nil, "", 0, false, "", err
			}
			argsSlice = parsed
		}
		if d, ok := v["cwd"].(string); ok && cwd == "" {
			cwd = filepath.Clean(d)
			if absCwd, err := filepath.Abs(cwd); err == nil {
				cwd = absCwd
			}
		}
		if t, ok := v["timeout"].(string); ok {
			if d, e := time.ParseDuration(t); e == nil && timeout == h.defaultTimeout {
				timeout = d
			}
		}
	}
	if command == "" {
		return "", nil, "", 0, false, "", errors.New("local_shell: command is required (tools.args.command or input)")
	}
	return command, argsSlice, cwd, timeout, useShell, stdin, nil
}

func (h *LocalExecTools) checkAllowlist(command string, useShell bool, allowedCommands []string, allowedDir string, deniedCommands []string) error {
	// Security: shell mode is refused whenever any policy is active. A raw
	// shell string cannot be statically checked for pipes (|), logic operators
	// (&&, ||) or subshells ($(...)), so allowing it would let a model escape
	// an allowlist/denylist/allowed-dir policy via shell injection, e.g. with
	// _allowed_commands=git and {"command":"git status; rm -rf /","shell":true}.
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
	// 2. Allowlist checks (only enforced when configured; otherwise authorization
	// is the responsibility of upstream layers — typically the HITL wrapper).
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

type capWriter struct {
	buf       bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func (cw *capWriter) Write(p []byte) (n int, err error) {
	if cw.limit > 0 {
		if cw.written >= cw.limit {
			cw.truncated = true
			return 0, io.ErrShortWrite
		}
		writeLen := int64(len(p))
		if cw.written+writeLen > cw.limit {
			writeLen = cw.limit - cw.written
			cw.buf.Write(p[:writeLen])
			cw.written += writeLen
			cw.truncated = true
			return int(writeLen), io.ErrShortWrite
		}
	}
	n, err = cw.buf.Write(p)
	cw.written += int64(n)
	return n, err
}

func (h *LocalExecTools) run(ctx context.Context, command string, argsSlice []string, cwd string, timeout time.Duration, useShell bool, stdinStr string) (*LocalExecResult, error) {
	start := time.Now()
	shell := h.shell.WithDefaults()
	result := &LocalExecResult{Command: command, Shell: shell.Summary(), OS: shell.OS}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	limit := int64(2 * 1024 * 1024) // 2MB default fallback if no budget is provided
	if val, ok := ctx.Value(taskengine.ContextKeyOutputByteLimit).(int64); ok {
		limit = val
	}

	// Never truncate silently: the spool writers keep a bounded 20%-head/80%-tail
	// slice for the inline result and spill the full stream to a durable file so
	// the truncated result can name it concretely ("full output: <path>").
	// Errors cluster at the tail, so the 80% weight is on the end of the stream.
	stdout := newSpoolWriter(ctx, "local_shell-stdout", limit)
	stderr := newSpoolWriter(ctx, "local_shell-stderr", limit)

	spec := CommandSpec{
		Command:  command,
		Args:     argsSlice,
		Cwd:      cwd,
		Timeout:  timeout,
		UseShell: useShell,
		Shell:    shell,
		Stdin:    stdinStr,
	}
	exitCode, runErr := h.runner.Run(runCtx, spec, stdout, stderr)
	result.DurationSeconds = time.Since(start).Seconds()

	if errors.Is(runErr, ErrOutputBudgetExceeded) {
		// Backend signalled it had already truncated its own output stream. The
		// partial bytes are from an incomplete write and must not be surfaced or
		// spooled (a poisoned partial is worse than nothing).
		stdout.discard()
		stderr.discard()
		result.Success = false
		result.ExitCode = -1
		result.Error = fmt.Sprintf("Output truncated: command exceeded the context budget (%d bytes). Re-run with a narrower scope or redirect output to a file. %s", limit, severityRecoverable)
		return result, nil
	}

	stdoutPath := stdout.close()
	stderrPath := stderr.close()

	result.Stdout = strings.TrimRight(stdout.inlineOutput(), "\r\n")
	result.Stderr = strings.TrimRight(stderr.inlineOutput(), "\r\n")

	if stdout.truncated() || stderr.truncated() {
		// Never silent: the inline Stdout/Stderr already carry the 20/80 split with
		// an embedded spool pointer; the Error field names the concrete next step
		// and its severity. A spool that could not be written is the only
		// genuinely fatal case here (disk full / spool unwritable).
		result.Success = false
		result.ExitCode = -1
		var parts []string
		if stdout.truncated() {
			parts = append(parts, spoolNotice("stdout", stdout, stdoutPath, limit))
		}
		if stderr.truncated() {
			parts = append(parts, spoolNotice("stderr", stderr, stderrPath, limit))
		}
		result.Error = strings.Join(parts, " ") + " " + truncationSeverity(stdout, stderr)
		return result, nil
	}

	if runErr != nil {
		result.Error = runErr.Error() + " " + severityRecoverable
		result.Success = false
		result.ExitCode = exitCode
		return result, nil
	}
	result.ExitCode = exitCode
	result.Success = exitCode == 0
	if !result.Success && result.Error == "" {
		result.Error = fmt.Sprintf("command exited with status %d %s", exitCode, severityRecoverable)
	}
	return result, nil
}

// spoolNotice renders the truncation notice for one stream: it names the exact
// retrieval step (the spool file path and full size), and always contains the
// phrase "context budget" so callers keying on it keep working.
func spoolNotice(stream string, w *spoolWriter, path string, budget int64) string {
	if w.spoolErr != nil {
		return fmt.Sprintf("Output truncated: %s exceeded the context budget (%d bytes); the full output could not be spooled (%v).", stream, budget, w.spoolErr)
	}
	if w.overCap {
		return fmt.Sprintf("Output truncated: %s exceeded the context budget (%d bytes); showing the first 20%% and last 80%%; full output (first %s of a larger stream): %s.", stream, budget, humanSize(w.spooled), path)
	}
	return fmt.Sprintf("Output truncated: %s exceeded the context budget (%d bytes); showing the first 20%% and last 80%%; full output: %s (%s).", stream, budget, path, humanSize(w.total))
}

// truncationSeverity returns the fatal marker when a spool could not be written
// (disk full / spool unwritable), otherwise the recoverable marker — a
// too-large output is fixable by narrowing the command.
func truncationSeverity(stdout, stderr *spoolWriter) string {
	if stdout.spoolErr != nil || stderr.spoolErr != nil {
		reason := "spool unwritable"
		if isDiskFull(stdout.spoolErr) || isDiskFull(stderr.spoolErr) {
			reason = "disk full"
		}
		return "(fatal: " + reason + ")"
	}
	return severityRecoverable
}

// splitShellArgs splits a string into an array of shell arguments, respecting single and double quotes and basic backslash escapes.
func splitShellArgs(s string) []string {
	var args []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	for _, r := range s {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if (r == ' ' || r == '\t' || r == '\n') && !inSingle && !inDouble {
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// Supports implements taskengine.ToolsRegistry.
func (h *LocalExecTools) Supports(ctx context.Context) ([]string, error) {
	return []string{localExecToolsName}, nil
}

// GetSchemasForSupportedTools implements taskengine.ToolsWithSchema.
func (h *LocalExecTools) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	shellDesc := h.shell.ShellModeDescription()
	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info:    &openapi3.Info{Title: "Local Exec Tools", Description: "Run commands on the local host. " + shellDesc, Version: "1.0.0"},
		Paths:   openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: map[string]*openapi3.SchemaRef{
				"LocalExecRequest": {
					Value: &openapi3.Schema{
						Type: &openapi3.Types{openapi3.TypeObject},
						Properties: map[string]*openapi3.SchemaRef{
							"command": {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "Executable path or name"}},
							"args": {Value: &openapi3.Schema{
								OneOf: []*openapi3.SchemaRef{
									{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "Space-separated arguments string"}},
									{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeArray}, Items: &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}}}, Description: "Array of argument strings"}},
								},
								Description: "Arguments as space-separated string or array of strings",
							}},
							"cwd":     {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "Working directory"}},
							"timeout": {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: "Duration e.g. 30s"}},
							"shell":   {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeBoolean}, Description: shellDesc}},
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
							"shell":            {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}}},
							"os":               {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}}},
						},
					},
				},
			},
		},
	}
	return map[string]*openapi3.T{localExecToolsName: schema}, nil
}

// GetToolsForToolsByName implements taskengine.ToolsWithSchema.
// Chain-level policy (allowed/denied commands, allowed dir) is read from ctx
// via ToolsArgsFromContext when present, falling back to the struct defaults.
func (h *LocalExecTools) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != localExecToolsName {
		return nil, fmt.Errorf("unknown tools: %s", name)
	}
	allowedCommands, allowedDir, deniedCommands := h.resolvePolicy(ctx)
	shellDesc := h.shell.ShellModeDescription()
	desc := "Run a terminal command on the local host. Returns {stdout, stderr, exitCode, success, durationSeconds}. Output is capped at the remaining context budget; when it exceeds the cap the FULL output is spooled to a file and stdout/stderr instead carry a 20%-head/80%-tail slice (errors cluster at the tail) with an inline pointer, while the error field names the spool concretely ('full output: <path> (N KiB)') so nothing is lost silently. Errors carry a severity marker you can key on: '(recoverable: adjust parameters and retry)' for anything a corrected call fixes (output too large, bad command, denied path), and '(fatal: <reason>)' only when the environment is broken (disk full, spool unwritable) and retrying will not help. For file operations prefer local_fs.* tools: read_file, write_file, sed, find_files. They enforce sandbox boundaries, size limits, and a read-before-write contract that local_shell does not. Use local_shell for operations with no dedicated tool: running tests, builds, git commands, environment inspection. " + shellDesc
	if len(allowedCommands) > 0 {
		desc += " Allowed commands: " + strings.Join(allowedCommands, ", ") + "."
	}
	if allowedDir != "" {
		desc += " Commands must reside under: " + allowedDir + "."
	}
	if len(deniedCommands) > 0 {
		desc += " Denied commands: " + strings.Join(deniedCommands, ", ") + "."
	}

	// When any policy constraint is active, shell mode is rejected at execution
	// time. Keep the schema provider-compatible and communicate the restriction
	// in prose; Gemini rejects boolean enum values in tool declarations.
	policyActive := len(allowedCommands) > 0 || allowedDir != "" || len(deniedCommands) > 0
	var shellProp map[string]interface{}
	if policyActive {
		shellProp = map[string]interface{}{
			"type":        "boolean",
			"description": "Shell mode is disabled by the active command policy. Omit or set false; provide command and args as separate parameters.",
		}
	} else {
		shellProp = map[string]interface{}{
			"type":        "boolean",
			"description": shellDesc,
		}
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
							"oneOf": []interface{}{
								map[string]interface{}{
									"type":        "string",
									"description": "Space-separated arguments string",
								},
								map[string]interface{}{
									"type":        "array",
									"items":       map[string]interface{}{"type": "string"},
									"description": "Array of argument strings",
								},
							},
							"description": "Arguments as space-separated string or array of strings",
						},
						"cwd": map[string]interface{}{
							"type":        "string",
							"description": "Working directory",
						},
						"timeout": map[string]interface{}{
							"type":        "string",
							"description": "Duration e.g. 30s",
						},
						"shell": shellProp,
					},
					"required": []string{"command"},
				},
			},
		},
	}, nil
}

var _ taskengine.ToolsRepo = (*LocalExecTools)(nil)
