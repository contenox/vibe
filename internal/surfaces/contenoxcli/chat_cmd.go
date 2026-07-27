// chat_cmd.go implements contenox-runtime chat (session-backed chain execution).
package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/services/localtools"
)

// chatOpts carries all effective config and flags needed by the run pipeline.
type chatOpts struct {
	EffectiveDB                  string
	EffectiveChain               string
	EffectiveDefaultModel        string
	EffectiveDefaultProvider     string
	EffectiveConfiguredModel     string
	EffectiveConfiguredProvider  string
	EffectiveAltDefaultModel     string
	EffectiveAltDefaultProvider  string
	EffectiveMaxTokens           string
	EffectiveContext             int
	EffectiveNoDeleteModels      bool
	EffectiveEnableLocalExec     bool
	EffectiveLocalExecAllowedDir string
	EffectiveTracing             bool
	EffectiveSteps               bool
	EffectiveHITL                bool
	EffectiveRaw                 bool
	EffectiveThink               string
	HistoryTrim                  int
	LastN                        int
	InputValue                   string
	InputFlagPassed              bool
	// AttachPaths are --attach image files; they ride the turn's user message
	// as ImageParts and route the request to a CanVision provider.
	AttachPaths []string
	ContenoxDir string
	// EffectiveSkipBackendCycle skips state.RunBackendCycle (e.g. contenox-runtime doctor --skip-cycle).
	EffectiveSkipBackendCycle bool
	// EffectiveAskApproval lets editor integrations reuse BuildEngine while
	// supplying their own HITL UI instead of the CLI tty prompt.
	EffectiveAskApproval localtools.AskApproval
	// EffectiveTaskEventSink lets editor integrations receive task events
	// directly without subscribing to the engine bus.
	EffectiveTaskEventSink taskengine.TaskEventSink
	// WarnW is where engine construction prints the messages an OPERATOR has
	// to read and act on (today: the ungated-local_shell posture in
	// localToolset). It is not an instrumentation seam — telemetry goes to the
	// tracker — it is the command's own stderr, carried this far down because
	// BuildEngine has nine call sites and only the command layer knows which
	// writer is the operator's.
	//
	// A nil writer means "nobody is listening": tests and editor-embedded
	// callers get silence rather than a line on some unrelated stream.
	WarnW io.Writer
}

// execChat runs the full chat pipeline and returns any error encountered.
// db is already opened by the caller (runChat in cli.go) so we share it here.
func execChat(ctx context.Context, db libdb.DBManager, opts chatOpts, out, errW io.Writer) error {
	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	if err := PreflightLLMSetup(errW, engine.SetupCheck); err != nil {
		return err
	}

	// ------------------------------------------------------------------------
	// 10. Load chain from file
	// ------------------------------------------------------------------------
	chainPathAbs, err := filepath.Abs(opts.EffectiveChain)
	if err != nil {
		return fmt.Errorf("invalid chain path: %w", err)
	}
	chain, err := loadChainFromFile(chainPathAbs)
	if err != nil {
		return err
	}

	// Determine input: from flag, positional args (+optional stdin), or stdin alone.
	in := opts.InputValue
	if !opts.InputFlagPassed {
		stdinData, ok, err := readStdinIfAvailable(maxCLIStdinBytes)
		if err != nil {
			return err
		}
		stdinStr := strings.TrimSpace(stdinData)
		if ok && stdinStr != "" {
			if in != "" {
				in = in + "\n\n" + stdinStr
			} else {
				in = stdinStr
			}
		}
	}
	images, err := loadImageAttachments(opts.AttachPaths)
	if err != nil {
		return err
	}
	// An image-only turn is valid ("what is this?" asked by attachment alone);
	// no input AND no attachment is not.
	if in == "" && len(images) == 0 {
		return fmt.Errorf("no input for chain: pass input as args, --input, or pipe via stdin")
	}

	// ------------------------------------------------------------------------
	// 11. Build agent and execute via service layer
	// ------------------------------------------------------------------------
	workspaceID := ResolveWorkspaceID(opts.ContenoxDir)

	// Resolve session
	sessionReportErr, _, sessionEnd := engine.Tracker.Start(ctx, "resolve", "active_session")
	sessionID, err := ensureDefaultSession(ctx, db, workspaceID)
	if err != nil {
		sessionReportErr(err)
		fmt.Fprintf(errW, "warning: failed to resolve active session — history will not be persisted: %v\n", err)
		sessionID = ""
	}
	sessionEnd()

	templateVars := buildTemplateVars(opts)

	// Create agent using new Engine-based Deps.
	ag := agentservice.New(agentservice.Deps{
		Engine:      engine,
		DB:          db,
		WorkspaceID: workspaceID,
	})

	// The chain run is a tracked OPERATION, not a log line: engine.Tracker is a
	// log-backed tracker exactly when --trace is on and a Noop otherwise, so
	// this replaces the old `if tracing { slog.Info(...) }` with the same
	// condition expressed through the one instrumentation seam — and gains the
	// completion record and duration that the bare Info line never had.
	_, _, chainEnd := engine.Tracker.Start(ctx, "execute", "chain", "chain", chainPathAbs)
	defer chainEnd()
	if !opts.EffectiveTracing {
		fmt.Fprintln(errW, "Thinking...")
	}

	stopTrace := startTraceStream(ctx, opts, engine, errW)
	defer stopTrace()
	stopThoughts := startThoughtStream(ctx, engine, errW, opts.EffectiveThink)
	defer stopThoughts()

	agentsMD, agentsMDSource := loadAgentsMDFromCwd()

	resp, err := ag.Prompt(ctx, agentservice.PromptRequest{
		SessionID:      sessionID,
		Input:          in,
		Images:         images,
		Chain:          chain,
		TemplateVars:   templateVars,
		ContextLength:  opts.EffectiveContext,
		HistoryTrim:    opts.HistoryTrim,
		AgentsMD:       agentsMD,
		AgentsMDSource: agentsMDSource,
	})
	if err != nil {
		if isModelResolverFailure(err) {
			PrintSetupIssues(errW, engine.SetupCheck)
		}
		return fmt.Errorf("chain execution failed: %w", err)
	}

	// ------------------------------------------------------------------------
	// 12. Print results (CLI-specific output formatting)
	// ------------------------------------------------------------------------
	if shouldPrintThinking(opts.EffectiveThink) {
		if hist, ok := resp.Output.(taskengine.ChatHistory); ok {
			for _, msg := range hist.Messages {
				if msg.Role == "assistant" && msg.Thinking != "" {
					fmt.Fprintln(errW, "\nReasoning:")
					fmt.Fprintln(errW, msg.Thinking)
				}
			}
		}
	}
	printRelevantOutput(out, resp.Output, resp.OutputType, opts.EffectiveRaw)

	// --last N: print last N non-system messages from the updated history.
	if opts.LastN > 0 {
		if hist, ok := resp.Output.(taskengine.ChatHistory); ok {
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
	if opts.EffectiveSteps && len(resp.Steps) > 0 {
		fmt.Fprintln(errW, "\n📋 Steps:")
		for i, u := range resp.Steps {
			fmt.Fprintf(errW, "  %d. %s (%s) %s %s\n", i+1, u.TaskID, u.TaskHandler, formatDuration(u.Duration), u.Transition)
		}
	}
	return nil
}

func buildTemplateVars(opts chatOpts) map[string]string {
	templateVars := map[string]string{
		"model":    opts.EffectiveDefaultModel,
		"provider": opts.EffectiveDefaultProvider,
		"think":    opts.EffectiveThink,
	}

	defaultModel := opts.EffectiveConfiguredModel
	if defaultModel == "" {
		defaultModel = opts.EffectiveDefaultModel
	}
	if defaultModel != "" {
		templateVars["default_model"] = defaultModel
	}

	defaultProvider := opts.EffectiveConfiguredProvider
	if defaultProvider == "" {
		defaultProvider = opts.EffectiveDefaultProvider
	}
	if defaultProvider != "" {
		templateVars["default_provider"] = defaultProvider
	}

	if opts.EffectiveAltDefaultModel != "" {
		templateVars["alt_model"] = opts.EffectiveAltDefaultModel
	}
	if opts.EffectiveAltDefaultProvider != "" {
		templateVars["alt_provider"] = opts.EffectiveAltDefaultProvider
	}
	if opts.EffectiveMaxTokens != "" {
		templateVars["max_tokens"] = opts.EffectiveMaxTokens
	}
	return templateVars
}
