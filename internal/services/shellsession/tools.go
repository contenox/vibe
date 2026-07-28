package shellsession

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/getkin/kin-openapi/openapi3"
)

// Tool names. The provider ("shell_session") is one ToolsRepo exposing two
// function tools. Run is gated by the same HITL machinery that wraps every tool
// (default policy → approve); Read is ungated by policy (reference-only reads).
const (
	ToolsProviderName = "shell_session"
	ToolRun           = "shell_session_run"
	ToolRead          = "shell_session_read"
)

// RunResultJSON is the structured result the agent receives from a run: a marker
// and the initial output snapshot. The agent polls shell_session_read with the
// returned offset to follow long-running commands.
type RunResultJSON struct {
	Offset  int64  `json:"offset"`
	Output  string `json:"output"`
	Started bool   `json:"started_new_shell,omitempty"`
	Note    string `json:"note,omitempty"`
}

// ReadResultJSON is the structured result of a scrollback read.
type ReadResultJSON struct {
	Content    string `json:"content"`
	FromOffset int64  `json:"from_offset"`
	NextOffset int64  `json:"next_offset"`
	Exists     bool   `json:"exists"`
	Note       string `json:"note,omitempty"`
}

// tools implements taskengine.ToolsRepo backed by a Manager. It resolves the
// session id from the execution context (set by the engine per turn), so the
// same shared tool instance drives the right per-session shell.
type tools struct {
	mgr Manager
}

// NewTools returns the shell_session ToolsRepo. Register it in the engine's
// LocalTools map under ToolsProviderName exactly like local_shell/local_fs, so
// it is HITL-wrapped and reachable to the agent only when shell tooling is on.
func NewTools(mgr Manager) taskengine.ToolsRepo {
	return &tools{mgr: mgr}
}

func (h *tools) Exec(ctx context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if call == nil {
		return nil, taskengine.DataTypeAny, errors.New("shell_session: tools required")
	}
	toolName := call.ToolName
	if toolName == "" {
		toolName = call.Name
	}
	sessionID := sessionIDFromCtx(ctx)
	if sessionID == "" {
		return nil, taskengine.DataTypeAny, errors.New("shell_session: no chat session in context")
	}
	switch toolName {
	case ToolRun, "run":
		return h.execRun(ctx, sessionID, input, call)
	case ToolRead, "read":
		return h.execRead(sessionID, input, call)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("shell_session: unknown tool %q", toolName)
	}
}

func (h *tools) execRun(ctx context.Context, sessionID string, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	command := stringArg(input, call, "command")
	if command == "" {
		// Allow the bare-string form execute_tool_calls sometimes passes.
		if s, ok := input.(string); ok {
			command = s
		}
	}
	if command == "" {
		return nil, taskengine.DataTypeAny, errors.New("shell_session: 'command' is required (one line to type into the shell)")
	}
	res, err := h.mgr.Run(ctx, sessionID, command)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("shell_session: run: %w", err)
	}
	out := RunResultJSON{Offset: res.Offset, Output: res.Snapshot, Started: res.Started}
	if res.Snapshot == "" {
		out.Note = "No output captured yet; the command may still be running. Poll shell_session_read with 'since': " + strconv.FormatInt(res.Offset, 10) + " to follow it."
	}
	return out, taskengine.DataTypeJSON, nil
}

func (h *tools) execRead(sessionID string, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	since, hasSince := intArg(input, call, "since")
	tail, hasTail := intArg(input, call, "tail_bytes")
	if !hasSince {
		if hasTail {
			since = -1 // tail mode
		} else {
			since = 0 // whole retained scrollback
		}
	}
	res := h.mgr.Read(sessionID, since, int(tail))
	out := ReadResultJSON{
		Content:    res.Content,
		FromOffset: res.FromOffset,
		NextOffset: res.NextOffset,
		Exists:     res.Exists,
	}
	if !res.Exists {
		out.Note = "No shell exists for this session yet. It is created on the first shell_session_run."
	}
	return out, taskengine.DataTypeJSON, nil
}

func (h *tools) Supports(context.Context) ([]string, error) {
	return []string{ToolsProviderName, ToolRun, ToolRead}, nil
}

func (h *tools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

func (h *tools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	runTool := taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name: ToolRun,
			Description: "Submit ONE command line to this chat's persistent shell (a real terminal rooted at the session workspace). " +
				"The shell keeps its working directory, environment, and history between calls, and long-running processes stay alive — a second run while one is still going types into the same running shell (that is normal shell stdin behavior, not an error). " +
				"Returns quickly with {offset, output} where output is a short initial snapshot, NOT the full result: it does not block until the command finishes. " +
				"To follow a command's ongoing output, call " + ToolRead + " with 'since' set to the returned offset. Requires approval under the active HITL policy, one approval per line.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{
						"type":        "string",
						"description": "The single command line to run in the shell, e.g. \"go test ./... 2>&1 | tail -n 40\".",
					},
				},
				"required": []string{"command"},
			},
		},
	}
	readTool := taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name: ToolRead,
			Description: "Read scrollback from this chat's persistent shell. Terminal output is never streamed into your context automatically — read it here when you need it. " +
				"Pass 'since' (an offset from a previous " + ToolRun + "/" + ToolRead + " result) to get only new output since that marker, or 'tail_bytes' to get the last N bytes. With neither, returns the full retained scrollback. " +
				"Returns {content, from_offset, next_offset}; use next_offset as the next 'since'. This read is not gated by HITL.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"since": map[string]interface{}{
						"type":        "integer",
						"description": "Return output at/after this scrollback offset (from a prior result's offset/next_offset).",
					},
					"tail_bytes": map[string]interface{}{
						"type":        "integer",
						"description": "When 'since' is omitted, return only the last N bytes of scrollback.",
					},
				},
			},
		},
	}
	switch name {
	case ToolRun:
		return []taskengine.Tool{runTool}, nil
	case ToolRead:
		return []taskengine.Tool{readTool}, nil
	case ToolsProviderName, "":
		return []taskengine.Tool{runTool, readTool}, nil
	default:
		return nil, fmt.Errorf("shell_session: unknown tool %q", name)
	}
}

func sessionIDFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	return v
}

// stringArg reads a string parameter from either the declared tools.Args map or
// the dynamic input map (execute_tool_calls).
func stringArg(input any, call *taskengine.ToolsCall, key string) string {
	if call != nil && call.Args != nil {
		if v := call.Args[key]; v != "" {
			return v
		}
	}
	if m, ok := input.(map[string]any); ok {
		if s, ok := m[key].(string); ok {
			return s
		}
	}
	return ""
}

// intArg reads an integer parameter from tools.Args or the dynamic input map,
// tolerating a JSON number or a numeric string.
func intArg(input any, call *taskengine.ToolsCall, key string) (int64, bool) {
	if call != nil && call.Args != nil {
		if v := call.Args[key]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n, true
			}
		}
	}
	if m, ok := input.(map[string]any); ok {
		switch v := m[key].(type) {
		case float64:
			return int64(v), true
		case int:
			return int64(v), true
		case int64:
			return v, true
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

var _ taskengine.ToolsRepo = (*tools)(nil)
