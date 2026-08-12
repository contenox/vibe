package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/vfs"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

type chatOpts struct {
	// EffectiveTracker, when non-nil, overrides the engine's tracker (Noop, or the log tracker under --trace).
	EffectiveTracker             libtracker.ActivityTracker
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
	// EffectiveOptInBeta gates beta feature registration: off leaves goja unregistered and the agent-* discovery convention narrowed to the shipped planner.
	EffectiveOptInBeta bool
	HistoryTrim        int
	LastN              int
	InputValue         string
	InputFlagPassed    bool
	// AttachPaths are --attach image files; they ride the turn's user message
	// as ImageParts and route the request to a CanVision provider.
	AttachPaths []string
	ContenoxDir string
	// EffectiveSkipBackendCycle skips state.RunBackendCycle (e.g. contenox-runtime doctor --skip-cycle).
	EffectiveSkipBackendCycle bool
	// EffectiveAskApproval lets editor integrations reuse BuildEngine while
	// supplying their own HITL UI instead of the CLI tty prompt.
	EffectiveAskApproval localtools.AskApproval
	// EffectiveHITLService is the hitlservice.Service BuildEngine gates this engine through instead of minting its own; nil mints one, and it is ignored when EffectiveHITL is false.
	EffectiveHITLService hitlservice.Service
	// EffectiveTaskEventSink lets editor integrations receive task events
	// directly without subscribing to the engine bus.
	EffectiveTaskEventSink taskengine.TaskEventSink
	// EffectiveExtraTools are host-scoped tool providers merged into this engine's toolset and nowhere else.
	EffectiveExtraTools map[string]taskengine.ToolsRepo
	// WarnW is where engine construction prints messages the operator must act on; nil means silence.
	WarnW io.Writer
	// EffectiveStreamOutput renders assistant prose to stdout as it arrives instead of only at the end.
	EffectiveStreamOutput bool
}

func execChat(ctx context.Context, db libdb.DBManager, opts chatOpts, out, errW io.Writer) error {
	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	if err := PreflightLLMSetup(errW, engine.SetupCheck); err != nil {
		return err
	}

	chainPathAbs, err := filepath.Abs(opts.EffectiveChain)
	if err != nil {
		return fmt.Errorf("invalid chain path: %w", err)
	}
	chain, err := loadChainFromFile(chainPathAbs)
	if err != nil {
		return err
	}

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
	if in == "" && len(images) == 0 {
		return fmt.Errorf("no input for chain: pass input as args, --input, or pipe via stdin")
	}

	workspaceID := ResolveWorkspaceID(opts.ContenoxDir)

	sessionReportErr, _, sessionEnd := engine.Tracker.Start(ctx, "resolve", "active_session")
	sessionID, err := ensureDefaultSession(ctx, db, workspaceID)
	if err != nil {
		sessionReportErr(err)
		fmt.Fprintf(errW, "warning: failed to resolve active session — history will not be persisted: %v\n", err)
		sessionID = ""
	}
	sessionEnd()

	templateVars := buildTemplateVars(opts)

	ag := agentservice.New(agentservice.Deps{
		Engine:      engine,
		DB:          db,
		WorkspaceID: workspaceID,
	})

	_, _, chainEnd := engine.Tracker.Start(ctx, "execute", "chain", "chain", chainPathAbs)
	defer chainEnd()
	if !opts.EffectiveTracing {
		fmt.Fprintln(errW, "Thinking...")
	}

	stopTrace := startTraceStream(ctx, opts, engine, errW)
	defer stopTrace()
	stopThoughts := startThoughtStream(ctx, engine, errW, opts.EffectiveThink)
	defer stopThoughts()
	// --raw asks for the structured payload verbatim, which prose deltas are
	// not a prefix of; that path stays buffered even on a terminal.
	live := startAssistantStream(ctx, engine, out, opts.EffectiveStreamOutput && !opts.EffectiveRaw)
	defer live.Stop()

	agentsMD, agentsMDSource := loadAgentsMDFromCwd()

	// The session's own cwd persists across resume, so relative paths resolve against where it started, not the resumer's directory.
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		ctx = vfs.WithSessionCwd(ctx, cwd)
	}

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
	// Closed before anything else prints: the deltas already on screen decide
	// what the buffered print still owes (printRemainingOutput).
	live.Stop()
	if err != nil {
		if isModelResolverFailure(err) {
			PrintSetupIssues(errW, engine.SetupCheck)
		}
		// agentservice already wraps chain failures; wrapping again doubles
		// the "chain execution failed" prefix.
		return err
	}

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
	printRemainingOutput(out, live.Written(), resp.Output, resp.OutputType, opts.EffectiveRaw)

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

const chatStreamDrainGrace = 250 * time.Millisecond

type assistantStream struct {
	mu      sync.Mutex
	written strings.Builder
	once    sync.Once
	stop    func()
}

func startAssistantStream(ctx context.Context, engine *Engine, w io.Writer, enabled bool) *assistantStream {
	s := &assistantStream{}
	if !enabled || engine == nil || engine.Bus == nil {
		return s
	}
	reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string)
	if !ok || reqID == "" {
		return s
	}
	subject := taskengine.TaskEventRequestSubject(reqID)

	tracker := engine.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reportErr, _, end := tracker.Start(ctx, "subscribe", "assistant_stream", "subject", subject)
	defer end()

	streamCtx, cancel := context.WithCancel(ctx)
	rawCh := make(chan []byte, 32)
	sub, err := engine.Bus.Stream(streamCtx, subject, rawCh)
	if err != nil {
		cancel()
		reportErr(err)
		return s
	}

	done := make(chan struct{})
	go func() {
		s.render(streamCtx, rawCh, w)
		close(done)
	}()
	s.stop = func() {
		time.Sleep(chatStreamDrainGrace)
		cancel()
		_ = sub.Unsubscribe()
		<-done
	}
	return s
}

func (s *assistantStream) render(ctx context.Context, ch <-chan []byte, w io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			var ev taskengine.TaskEvent
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			if ev.Kind != taskengine.TaskEventStepChunk || ev.Content == "" {
				continue
			}
			if !taskengine.IsAssistantProseHandler(ev.TaskHandler) {
				continue
			}
			s.mu.Lock()
			_, werr := io.WriteString(w, ev.Content)
			if werr == nil {
				s.written.WriteString(ev.Content)
			}
			s.mu.Unlock()
			if werr != nil {
				return
			}
		}
	}
}

// Stop drains and closes the subscription; idempotent, so the deferred call
// after an early return is a no-op once the happy path already stopped it.
func (s *assistantStream) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.stop != nil {
			s.stop()
		}
	})
}

// Written is everything this stream rendered; call after Stop, once the render goroutine has joined.
func (s *assistantStream) Written() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written.String()
}

func printRemainingOutput(w io.Writer, streamed string, output any, outputType taskengine.DataType, raw bool) {
	if streamed == "" || raw {
		printRelevantOutput(w, output, outputType, raw)
		return
	}
	// The two shapes printRelevantOutput renders as bare prose — the only ones
	// a prose delta can be part of.
	final := ""
	switch outputType {
	case taskengine.DataTypeChatHistory:
		if ch, ok := output.(taskengine.ChatHistory); ok {
			final = lastAssistantContentFromHistory(ch)
		}
	case taskengine.DataTypeString:
		if s, ok := output.(string); ok {
			final = s
		}
	}
	if final == "" {
		fmt.Fprintln(w)
		printRelevantOutput(w, output, outputType, raw)
		return
	}
	fmt.Fprintln(w, final[streamOverlap(streamed, final):])
}

func streamOverlap(streamed, final string) int {
	n := min(len(streamed), len(final))
	for ; n > 0; n-- {
		if strings.HasSuffix(streamed, final[:n]) {
			return n
		}
	}
	return 0
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
