package gojatool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
)

// ToolsProviderName is the tools-provider key this package registers under.
// Policy addressing is `tools: "goja", tool: <name>`.
const ToolsProviderName = "goja"

// ToolEval is the sandbox tool the model drives directly. Script tools are
// registered beside it under their own declared names, unprefixed — the
// provider is the namespace.
const ToolEval = "goja_eval"

// Config configures a Toolset. Zero values fall back to the documented
// defaults, so Config{} is a valid, safe configuration with no script tools.
type Config struct {
	// ScriptDir is the directory scanned for `*.js` script tools. Empty, or
	// absent on disk, means no script tools — goja_eval is still registered.
	// Anything else that goes wrong while reading it is a fail-fast startup
	// error.
	ScriptDir string

	// Host is the engine's real tool path — see HostToolCaller. May be nil
	// at construction and supplied later with SetHost; until set, host.tool
	// throws ErrHostUnavailable and scripts can only compute.
	Host HostToolCaller

	// Deadline is the default per-execution budget (default DefaultDeadline).
	Deadline time.Duration

	// MaxDeadline is the ceiling on any per-call or per-script override
	// (default and hard maximum MaxDeadline).
	MaxDeadline time.Duration

	// OutputCap bounds the marshaled result of one execution in bytes
	// (default DefaultOutputCap, ceiling maxOutputCap).
	OutputCap int

	// MaxCallStackSize bounds JS call depth (default defaultMaxCallStack).
	MaxCallStackSize int

	// ReservedNames are additional tool names a script may not claim.
	// goja_eval is always reserved.
	ReservedNames []string

	// Tracker is where load-time diagnostics are reported (see
	// reportUndeclaredReach). Nil degrades to libtracker.NoopTracker.
	Tracker libtracker.ActivityTracker
}

// Toolset is the taskengine.ToolsRepo for the goja provider: goja_eval plus
// one tool per loaded script. New returns the concrete type, rather than the
// interface, because the registration site needs SetHost and Shutdown, which
// the interface cannot carry.
type Toolset struct {
	sb      *sandbox
	scripts []*Script
	byName  map[string]*Script
	names   []string
}

var _ taskengine.ToolsRepo = (*Toolset)(nil)

// New builds the goja toolset, loading and validating every script tool. A
// single bad script fails construction.
func New(cfg Config) (*Toolset, error) {
	sb := newSandbox(cfg.Deadline, cfg.MaxDeadline, cfg.OutputCap, cfg.MaxCallStackSize)
	sb.host = cfg.Host
	tracker := cfg.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}

	ctx := context.Background()
	reserved := append([]string{ToolEval}, cfg.ReservedNames...)
	scripts, err := sb.loadScripts(ctx, cfg.ScriptDir, reserved)
	if err != nil {
		return nil, err
	}

	t := &Toolset{
		sb:      sb,
		scripts: scripts,
		byName:  make(map[string]*Script, len(scripts)),
		names:   make([]string, 0, len(scripts)+1),
	}
	t.names = append(t.names, ToolEval)
	for _, sc := range scripts {
		t.byName[sc.Name] = sc
		t.names = append(t.names, sc.Name)
	}
	reportUndeclaredReach(ctx, tracker, scripts)
	return t, nil
}

// reportUndeclaredReach says once, at startup, which scripts declare no
// `tools` list — a diagnostic, not an error, since the field is optional.
// Reported through the tracker rather than a log call of its own, since the
// tracker is this repo's single instrumentation seam.
func reportUndeclaredReach(ctx context.Context, tracker libtracker.ActivityTracker, scripts []*Script) {
	var undeclared []string
	for _, sc := range scripts {
		if !sc.ToolsDeclared {
			undeclared = append(undeclared, fmt.Sprintf("%s (%s)", sc.Name, sc.File))
		}
	}
	if len(undeclared) == 0 {
		return
	}
	reportErr, _, end := tracker.Start(ctx, "load", "goja_script_tools",
		"count", len(undeclared),
		"scripts", strings.Join(undeclared, ", "),
		"repair", `add tools: ["provider.tool_name", ...] to the tool descriptor so an approval card can say what the script will touch`)
	reportErr(fmt.Errorf("goja script tools declare no `tools` list, so they may call any tool the envelope allows"))
	end()
}

// Shutdown refuses further executions and joins in-flight ones. Chain it onto
// engine.Stop. Safe to call more than once.
func (t *Toolset) Shutdown() { t.sb.shutdown() }

// Scripts returns the loaded script tools, in load order. An approval
// surface uses Script.Tools together with Script.ToolsDeclared to answer what
// a script will touch — read both, since "declares it reaches nothing" and
// "declares nothing" both present as an empty list.
func (t *Toolset) Scripts() []*Script {
	out := make([]*Script, len(t.scripts))
	copy(out, t.scripts)
	return out
}

// Supports names the provider and every tool on it.
func (t *Toolset) Supports(context.Context) ([]string, error) {
	return append([]string{ToolsProviderName}, t.names...), nil
}

// Exec dispatches one tool call, stamping every returned error with the
// fatal-vs-recoverable severity marker.
func (t *Toolset) Exec(ctx context.Context, startTime time.Time, input any, debug bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	res, dt, err := t.execDispatch(ctx, startTime, input, debug, call)
	return res, dt, markSeverity(err)
}

// execDispatch resolves the tool and runs it. Argument names are strict
// (rejectUnknownArgs); argument values are coerced, since small models
// routinely emit JSON scalars as strings.
func (t *Toolset) execDispatch(ctx context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if call == nil {
		return nil, taskengine.DataTypeAny, errors.New("goja: tools required")
	}
	args := callArgs(input, call)
	toolName := call.ToolName
	if toolName == "" {
		toolName = call.Name
	}

	switch toolName {
	case ToolEval:
		res, err := t.evalTool(ctx, args)
		if err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return res, taskengine.DataTypeJSON, nil

	default:
		sc, ok := t.byName[toolName]
		if !ok {
			return nil, taskengine.DataTypeAny, recoverablef("goja: unknown tool %s; this provider offers %s",
				echoArg(toolName), strings.Join(t.names, ", "))
		}
		if err := t.checkScriptArgs(sc, args); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		res, err := t.sb.execScript(ctx, sc, args)
		if err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return res, taskengine.DataTypeJSON, nil
	}
}

// evalTool runs goja_eval.
func (t *Toolset) evalTool(ctx context.Context, args map[string]any) (*Result, error) {
	if err := rejectUnknownArgs(ToolEval, args, "code", "deadline_ms"); err != nil {
		return nil, err
	}
	code := argString(args, "code")
	if strings.TrimSpace(code) == "" {
		return nil, recoverablef("goja: %s needs `code`: a JavaScript program whose last expression is the result. Example: code: \"[1,2,3].map(n => n * 2)\"", ToolEval)
	}

	deadline := time.Duration(0)
	if raw, ok := args["deadline_ms"]; ok && raw != nil {
		ms, ok := argInt(args, "deadline_ms")
		if !ok || ms <= 0 {
			return nil, recoverablef("goja: %s: deadline_ms must be a positive number of milliseconds (default %d, ceiling %d), got %s",
				ToolEval, t.sb.deadline.Milliseconds(), t.sb.maxDeadline.Milliseconds(), echoArg(fmt.Sprint(raw)))
		}
		deadline = time.Duration(ms) * time.Millisecond
	}
	return t.sb.runSource(ctx, ToolEval, code, t.sb.clampDeadline(deadline))
}

// checkScriptArgs enforces the script's own declared schema at the boundary:
// unknown names are refused (unless the schema opts into
// additionalProperties) and missing required names are named.
func (t *Toolset) checkScriptArgs(sc *Script, args map[string]any) error {
	if !sc.allowExtra {
		if err := rejectUnknownArgs(sc.Name, args, sc.properties...); err != nil {
			return err
		}
	}
	var missing []string
	for _, name := range sc.required {
		v, ok := args[name]
		if !ok || v == nil || v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return recoverablef("%s: missing required argument(s): %s", sc.Name, strings.Join(missing, ", "))
	}
	return nil
}

// --- Argument decoding -------------------------------------------------------
// Accepts args from the chain input map or from the declarative
// ToolsCall.Args, rejects unknown argument names per tool, coerces values
// generously.

// callArgs assembles the argument map from the chain input or, for
// declarative `tools` tasks that carry arguments on the call itself, from
// ToolsCall.Args.
func callArgs(input any, call *taskengine.ToolsCall) map[string]any {
	if m, ok := input.(map[string]any); ok && len(m) > 0 {
		return m
	}
	if len(call.Args) > 0 {
		out := make(map[string]any, len(call.Args))
		for k, v := range call.Args {
			out[k] = v
		}
		return out
	}
	if m, ok := input.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func rejectUnknownArgs(toolName string, args map[string]any, allowed ...string) error {
	if len(args) == 0 {
		return nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	var unknown []string
	for key := range args {
		if _, ok := allowedSet[key]; !ok {
			// The key is model-supplied too, so it is clamped like any other
			// echoed argument.
			unknown = append(unknown, echoName(key))
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	sorted := append([]string(nil), allowed...)
	sort.Strings(sorted)
	shown := strings.Join(sorted, ", ")
	if shown == "" {
		shown = "none — this tool takes no arguments"
	}
	return fmt.Errorf("%s: unknown argument(s): %s (allowed: %s) %s",
		toolName, strings.Join(unknown, ", "), shown, severityRecoverable)
}

// echoName renders a model-supplied argument name for an error message:
// clamped, non-printable runes replaced rather than embedded.
func echoName(s string) string {
	var b strings.Builder
	for i, r := range []rune(s) {
		switch {
		case i >= maxEchoRunes:
			b.WriteString("…")
			return b.String()
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			b.WriteRune('?')
		}
	}
	return b.String()
}

func argString(args map[string]any, key string) string {
	x, ok := args[key]
	if !ok || x == nil {
		return ""
	}
	switch v := x.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

const (
	maxInt = int(^uint(0) >> 1)
	minInt = -maxInt - 1
)

// intFromFloat converts a JSON number to an int without the undefined
// behaviour Go's float-to-int conversion has outside the integer range.
// Out-of-range saturates; NaN reads as "no value".
func intFromFloat(f float64) (int, bool) {
	switch {
	case f != f:
		return 0, false
	case f >= float64(maxInt):
		return maxInt, true
	case f <= float64(minInt):
		return minInt, true
	}
	return int(f), true
}

func argInt(args map[string]any, key string) (int, bool) {
	x, ok := args[key]
	if !ok || x == nil {
		return 0, false
	}
	switch v := x.(type) {
	case float64:
		return intFromFloat(v)
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return intFromFloat(f)
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if n, err := strconv.Atoi(s); err == nil {
			return n, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return intFromFloat(f)
		}
	}
	return 0, false
}
