// run.go contains the main execution pipeline for the vibe CLI (steps 1–12).
package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/contenox/chatservice"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/localhooks"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/taskengine"
)

// chatOpts carries all effective config and flags needed by the run pipeline.
type chatOpts struct {
	EffectiveDB                       string
	EffectiveChain                    string
	EffectiveDefaultModel             string
	EffectiveDefaultProvider          string
	EffectiveContext                  int
	EffectiveNoDeleteModels           bool
	EffectiveEnableLocalExec          bool
	EffectiveLocalExecAllowedDir      string
	EffectiveTracing                  bool
	EffectiveSteps                    bool
	EffectiveRaw                      bool
	EffectiveThink                    bool
	HistoryTrim                       int
	LastN                             int
	InputValue                        string
	InputFlagPassed                   bool
	ContenoxDir                       string
	// AskApproval is an optional HITL callback. When non-nil, BuildEngine wraps
	// local_fs and local_shell with a HITLWrapper that requests human approval
	// before executing mutating or shell tools. Used only by the vibe TUI.
	AskApproval localhooks.AskApproval
	// PlannerChain and ExecutorChain, when both non-nil, cause BuildEngine to
	// register the plan_manager hook so the chat LLM can create and run plans
	// via tool calls. Used only by the vibe TUI.
	PlannerChain  *taskengine.TaskChainDefinition
	ExecutorChain *taskengine.TaskChainDefinition
}

// execChat runs the full chat pipeline and returns any error encountered.
// db is already opened by the caller (runChat in cli.go) so we share it here.
func execChat(ctx context.Context, db libdb.DBManager, opts chatOpts, out, errW io.Writer) error {
	// Component 1: use BuildEngine instead of the 150-line duplicate scaffold.
	// This fixes MCP being broken for `contenox chat` (the old code used
	// libbus.NewInMem() and never initialised mcpworker.Manager).
	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	// ------------------------------------------------------------------------
	// 10. Load chain from file
	// ------------------------------------------------------------------------
	chainPathAbs, err := filepath.Abs(opts.EffectiveChain)
	if err != nil {
		return fmt.Errorf("invalid chain path: %w", err)
	}
	chainData, err := os.ReadFile(chainPathAbs)
	if err != nil {
		return fmt.Errorf("failed to read chain file %q: %w", chainPathAbs, err)
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &chain); err != nil {
		return fmt.Errorf("failed to parse chain JSON: %w", err)
	}

	// Determine input: from flag, positional args (+optional stdin), or stdin alone.
	in := opts.InputValue
	if !opts.InputFlagPassed {
		if stat, err := os.Stdin.Stat(); err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
			stdinBytes, err := io.ReadAll(io.LimitReader(os.Stdin, 50<<20)) // cap at 50 MB
			if err != nil {
				return fmt.Errorf("failed to read from stdin: %w", err)
			}
			stdinStr := strings.TrimSpace(string(stdinBytes))
			if stdinStr != "" {
				if in != "" {
					in = in + "\n\n" + stdinStr
				} else {
					in = stdinStr
				}
			}
		}
	}
	if in == "" {
		return fmt.Errorf("no input for chain: pass input as args, --input, or pipe via stdin")
	}

	// ------------------------------------------------------------------------
	// 11. Execute chain
	// ------------------------------------------------------------------------
	templateVars := map[string]string{
		"model":    opts.EffectiveDefaultModel,
		"provider": opts.EffectiveDefaultProvider,
		"chain":    chain.ID,
	}
	if builtins := engine.JSEnv.GetBuiltinSignatures(); len(builtins) > 0 {
		var b strings.Builder
		for _, t := range builtins {
			if t.Function.Name != "console" {
				b.WriteString(t.Function.Name)
				b.WriteString(": ")
				b.WriteString(t.Function.Description)
				b.WriteString("\n")
			}
		}
		if hookTools, err := engine.JSEnv.GetExecuteHookToolDescriptions(ctx); err == nil && len(hookTools) > 0 {
			b.WriteString("executeHook tools: ")
			for i, t := range hookTools {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(t.Function.Name)
			}
		}
		templateVars["sandbox_api"] = b.String()
	}
	ctx = taskengine.WithTemplateVars(ctx, templateVars)

	// Persistent Session Management
	sessionID, err := ensureDefaultSession(ctx, db)
	if err != nil {
		slog.Warn("Failed to resolve active session — history will not be persisted", "error", err)
		sessionID = ""
	} else if sessionID != "" {
		// INJECT: Tunnel the session ID down the call stack so MCP workers can multiplex connections
		ctx = context.WithValue(ctx, runtimetypes.SessionIDContextKey, sessionID)
	}
	chatMgr := chatservice.NewManager(nil)

	var history []taskengine.Message
	if sessionID != "" {
		history, err = chatMgr.ListMessages(ctx, db.WithoutTransaction(), sessionID)
		if err != nil {
			slog.Warn("Failed to load chat history", "sessionID", sessionID, "error", err)
		}
	}

	// Apply --trim: cap history sent to model to last N messages.
	if opts.HistoryTrim > 0 && len(history) > opts.HistoryTrim {
		history = history[len(history)-opts.HistoryTrim:]
	}

	// Prepare Input
	userMsg := taskengine.Message{Role: "user", Content: in, Timestamp: time.Now().UTC()}
	chainInput := taskengine.ChatHistory{
		Messages: append(history, userMsg),
	}

	if opts.EffectiveTracing {
		slog.Info("Executing chain", "chain", chainPathAbs)
	} else {
		fmt.Fprintln(errW, "Thinking...")
	}
	output, outputType, stateUnits, err := engine.TaskService.Execute(ctx, &chain, chainInput, taskengine.DataTypeChatHistory)
	if err != nil {
		return fmt.Errorf("chain execution failed: %w", err)
	}

	// Persist Results Surgically.
	// Use context.WithoutCancel so a --timeout expiry doesn't lose the final message.
	if sessionID != "" && outputType == taskengine.DataTypeChatHistory {
		if updatedHistory, ok := output.(taskengine.ChatHistory); ok {
			cleanCtx := context.WithoutCancel(ctx)
			exec, commit, release, txErr := db.WithTransaction(cleanCtx)
			if txErr == nil {
				defer release()
				if err := chatMgr.PersistDiff(cleanCtx, exec, sessionID, updatedHistory.Messages); err != nil {
					slog.Error("Failed to persist chat diff", "sessionID", sessionID, "error", err)
				} else {
					if err := commit(cleanCtx); err != nil {
						slog.Error("Failed to commit chat persistence transaction", "error", err)
					}
				}
			} else {
				slog.Error("Failed to start transaction for chat persistence", "error", txErr)
			}
		} else {
			return fmt.Errorf("BUG: chain returned DataTypeChatHistory but output is not ChatHistory (type=%T)", output)
		}
	}

	// ------------------------------------------------------------------------
	// 12. Print results
	// ------------------------------------------------------------------------
	if opts.EffectiveThink {
		if hist, ok := output.(taskengine.ChatHistory); ok {
			for _, msg := range hist.Messages {
				if msg.Role == "assistant" && msg.Thinking != "" {
					fmt.Fprintln(errW, "\n💭 Reasoning:")
					fmt.Fprintln(errW, msg.Thinking)
				}
			}
		}
	}
	printRelevantOutput(out, output, outputType, opts.EffectiveRaw)

	// --last N: print last N non-system messages from the updated history.
	if opts.LastN > 0 {
		if hist, ok := output.(taskengine.ChatHistory); ok {
			var visible []taskengine.Message
			for _, m := range hist.Messages {
				if m.Role != "system" && m.Role != "tool" && len(m.CallTools) == 0 {
					visible = append(visible, m)
				}
			}
			if opts.LastN < len(visible) {
				visible = visible[len(visible)-opts.LastN:]
			}
			if len(visible) > 0 {
				fmt.Fprintln(errW, "\n── last", opts.LastN, "turns ──────────────────────")
				for _, m := range visible {
					fmt.Fprintf(errW, "[%s] %s:\n  %s\n\n", m.Timestamp.Format("15:04:05"), m.Role, m.Content)
				}
				fmt.Fprintln(errW, "────────────────────────────────────")
			}
		}
	}
	if opts.EffectiveSteps && len(stateUnits) > 0 {
		fmt.Fprintln(errW, "\n📋 Steps:")
		for i, u := range stateUnits {
			fmt.Fprintf(errW, "  %d. %s (%s) %s %s\n", i+1, u.TaskID, u.TaskHandler, formatDuration(u.Duration), u.Transition)
		}
	}
	return nil
}
