package shellsession

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// Tool names: shell_session is one ToolsRepo exposing two function tools — Run gated by HITL (default policy approve), Read ungated (reference-only).
const (
	ToolsProviderName = "shell_session"
	ToolRun           = "shell_session_run"
	ToolRead          = "shell_session_read"
)

// RunResultJSON is the structured result the agent receives from a run: a marker and the initial output snapshot, followed up via shell_session_read with the returned offset.
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

type tools struct {
	mgr Manager
}

// NewTools returns the shell_session ToolsRepo, registered in the engine's LocalTools map under ToolsProviderName like local_shell/local_fs so it is HITL-wrapped and reachable only when shell tooling is on.
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

func sessionIDFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	return v
}

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
