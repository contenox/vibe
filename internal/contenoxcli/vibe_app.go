package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/contenox/contenox/chatservice"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/localhooks"
	"github.com/contenox/contenox/planservice"
	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/taskengine"
)

// shellTimeout is the maximum time a $ shell command may run before it is
// cancelled. A blocked shell command would otherwise freeze the entire TUI.
const shellTimeout = 30 * time.Second

// ── Bubble Tea message types ──────────────────────────────────────────────────

type vibeLLMMsg struct {
	history taskengine.ChatHistory
	err     error
}

type vibeShellMsg struct {
	cmd    string
	stdout string
	stderr string
	err    error
}

type vibePlanCreatedMsg struct {
	plan  *planstore.Plan
	steps []*planstore.PlanStep
	err   error
}

type vibePlanLoadedMsg struct {
	plan  *planstore.Plan
	steps []*planstore.PlanStep
	err   error
}

type vibePlanStepMsg struct {
	step   *planstore.PlanStep
	status planstore.StepStatus
	result string
	err    error
	isAuto bool // true when dispatched from a /plan next --auto sequence
}

type vibePlanStepUpdateMsg struct {
	ordinal int
	status  planstore.StepStatus
	err     error
}

type vibePlanListMsg struct {
	lines []string
}

type vibePlanReplanMsg struct {
	steps []*planstore.PlanStep
	err   error
}

type vibeRunMsg struct {
	chainID string
	output  any
	err     error
}

type vibeMCPMsg struct {
	workers []string
}

// vibeHITLPromptMsg is sent from the AskApproval callback (running in a
// background hook goroutine) to the Bubble Tea event loop when an LLM tool
// call needs human approval. The background goroutine blocks on Response until
// the TUI sends true (approve) or false (deny).
type vibeHITLPromptMsg struct {
	Req      localhooks.ApprovalRequest
	Response chan bool
}

// ── Layout modes ──────────────────────────────────────────────────────────────

// layoutMode controls the sidebar layout. Ctrl+B cycles through the modes.
type layoutMode int

const (
	layoutNormal layoutMode = iota // sidebar proportional to terminal width
	layoutWide                     // sidebar at maximum width
	layoutFull                     // no sidebar — full chat area
	layoutModeCount
)

func (l layoutMode) String() string {
	switch l {
	case layoutNormal:
		return "normal"
	case layoutWide:
		return "wide"
	case layoutFull:
		return "full"
	}
	return "?"
}

// ── Model ─────────────────────────────────────────────────────────────────────

// vibeModel is the top-level Bubble Tea model for `contenox vibe`.
type vibeModel struct {
	ctx         context.Context
	engine      *Engine
	db          libdb.DBManager
	contenoxDir string

	// current chat chain + session
	chatChain *taskengine.TaskChainDefinition
	history   taskengine.ChatHistory
	sessionID string
	model     string
	provider  string

	// plan chains (set once at startup by ensurePlanChains)
	plannerChain  *taskengine.TaskChainDefinition
	executorChain *taskengine.TaskChainDefinition

	// active plan state (mirrored from SQLite)
	activePlan *planstore.Plan
	planSteps  []*planstore.PlanStep

	// UI
	viewport    viewport.Model
	input       textarea.Model
	width       int
	height      int
	sidebarW    int        // computed by relayout; 0 = hidden
	sidebarMaxH int        // max lines the sidebar may render (set by relayout)
	layout      layoutMode // current layout mode (cycles with Ctrl+B)

	// HITL approval state
	// Non-nil when a background tool call is waiting for the user to approve or
	// deny. The TUI intercepts y/n keystrokes and sends the answer on this channel.
	hitlApprovalCh chan bool

	// output log (every line appended; viewport renders it)
	log []string

	// sidebar
	mcpWorkers []string

	waiting bool
}

func newVibeModel(
	ctx context.Context,
	engine *Engine,
	db libdb.DBManager,
	sessionID string,
	contenoxDir string,
	chatChain *taskengine.TaskChainDefinition,
	plannerChain *taskengine.TaskChainDefinition,
	executorChain *taskengine.TaskChainDefinition,
	model string,
	provider string,
	initHistory taskengine.ChatHistory,
) vibeModel {
	ta := textarea.New()
	ta.Placeholder = "chat… or  $ cmd  or  /plan new <goal>  or  /plan next  or  /help"
	ta.Prompt = "" // Remove the internal vertical-bar cursor prompt that clashes with the custom UI
	ta.Focus()
	ta.SetWidth(80)
	ta.SetHeight(3)
	ta.CharLimit = 0
	ta.ShowLineNumbers = false

	// Pre-populate the log from the loaded history so previous messages are
	// visible in the viewport immediately on startup.
	var initLog []string
	for _, msg := range initHistory.Messages {
		switch msg.Role {
		case "user":
			initLog = append(initLog, vibeStyleUser.Render("›")+" "+msg.Content)
		case "assistant":
			if msg.Content != "" {
				initLog = append(initLog, vibeStyleAI.Render("·")+" "+msg.Content)
			}
		}
	}

	return vibeModel{
		ctx:           ctx,
		engine:        engine,
		db:            db,
		contenoxDir:   contenoxDir,
		chatChain:     chatChain,
		plannerChain:  plannerChain,
		executorChain: executorChain,
		model:         model,
		provider:      provider,
		sessionID:     sessionID,
		history:       initHistory,
		log:           initLog,
		viewport:      viewport.New(80, 20),
		input:         ta,
		// sidebarW is set on first WindowSizeMsg via relayout()
	}
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m vibeModel) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.loadActivePlan(),
		m.refreshMCP(),
	)
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m vibeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m = m.relayout()
		m.sync()

	case tea.KeyMsg:
		// When an HITL approval prompt is active, intercept y/n/enter before
		// normal key handling so the user can approve or deny the tool call.
		if m.hitlApprovalCh != nil {
			switch strings.ToLower(msg.String()) {
			case "y":
				m.push(vibeStyleHITL.Render("✓ Approved."))
				select {
				case m.hitlApprovalCh <- true:
				default:
					m.push(vibeStyleError.Render("✗ Tool timed out before approval was received."))
				}
				m.hitlApprovalCh = nil
				m.waiting = true
				m.sync()
				return m, nil
			case "n", "enter":
				m.push(vibeStyleHITL.Render("✗ Denied."))
				select {
				case m.hitlApprovalCh <- false:
				default:
					m.push(vibeStyleError.Render("✗ Tool timed out before denial was received."))
				}
				m.hitlApprovalCh = nil
				m.waiting = true
				m.sync()
				return m, nil
			default:
				return m, nil // ignore other keys while awaiting answer
			}
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+b":
			if !m.waiting {
				m.layout = (m.layout + 1) % layoutModeCount
				m = m.relayout()
				m.sync()
			}
		case "enter":
			if m.waiting {
				return m, nil
			}
			raw := strings.TrimSpace(m.input.Value())
			if raw == "" {
				return m, nil
			}
			m.input.Reset()
			return m.dispatch(raw)
		case "pgup":
			m.viewport.HalfViewUp()
		case "pgdown":
			m.viewport.HalfViewDown()
		}

	case vibeHITLPromptMsg:
		// A background hook goroutine needs human approval. Stop spinner, render
		// approvals info (including diff for file writes) and pause for y/n.
		m.waiting = false
		m.hitlApprovalCh = msg.Response
		m.push(vibeStyleHITL.Render(fmt.Sprintf("⚠️  Approval Required: [%s] %s", msg.Req.HookName, msg.Req.ToolName)))
		if msg.Req.Diff != "" {
			m.push(msg.Req.Diff)
		} else {
			// Show top-level args (truncated) when no diff is available.
			for k, v := range msg.Req.Args {
				m.push(fmt.Sprintf("  %s: %v", k, v))
			}
		}
		m.push(vibeStyleHITL.Render("Approve? [y/N]"))
		m.sync()
		return m, nil

	case vibeLLMMsg:
		m.waiting = false
		if msg.err != nil {
			m.push(vibeStyleError.Render("✗ " + msg.err.Error()))
		} else {
			m.history = msg.history
			if text := lastAssistantContentFromHistory(msg.history); text != "" {
				m.push(vibeStyleAI.Render("·") + " " + text)
			}
		}
		m.sync()

	case vibeShellMsg:
		m.waiting = false
		if msg.err != nil {
			// Bug B fix: show stdout first (may contain useful progress), then stderr.
			display := msg.stdout
			if msg.stderr != "" {
				if display != "" {
					display += "\nstderr: " + msg.stderr
				} else {
					display = "stderr: " + msg.stderr
				}
			}
			m.push(vibeStyleShell.Render("$") + " " + msg.cmd + "\n" + vibeStyleError.Render(display))
		} else {
			m.push(vibeStyleShell.Render("$") + " " + msg.cmd + "\n" + msg.stdout)
		}
		// Inject shell output into LLM context as a user message.
		// Bug B fix: include both stdout and stderr so the model has full context.
		var content string
		if msg.err != nil {
			if msg.stdout != "" {
				content = msg.stdout + "\nstderr: " + msg.stderr
			} else {
				content = "stderr: " + msg.stderr
			}
		} else {
			content = msg.stdout
		}
		m.history.Messages = append(m.history.Messages, taskengine.Message{
			// Inject as "user" role: strict LLM APIs (OpenAI, Anthropic) reject
			// "tool" messages that lack a preceding tool_calls array.
			Role: "user", Content: "Shell output:\n" + content,
		})
		// Bug A fix: persist shell output to SQLite so it survives Ctrl+C.
		if m.sessionID != "" && m.db != nil {
			chatMgr := chatservice.NewManager(nil)
			snap := m.history.Messages // capture before goroutine runs
			go func() {
				saveCtx := context.WithoutCancel(m.ctx)
				_ = withTransaction(saveCtx, m.db, func(tx libdb.Exec) error {
					return chatMgr.PersistDiff(saveCtx, tx, m.sessionID, snap)
				})
			}()
		}
		m.sync()

	case vibePlanCreatedMsg:
		m.waiting = false
		if msg.err != nil {
			m.push(vibeStyleError.Render("✗ plan: " + msg.err.Error()))
		} else {
			m.activePlan = msg.plan
			m.planSteps = msg.steps
			m.push(vibeStyleTool.Render(fmt.Sprintf("⊡ Created %q — %d steps. Use /plan next.", msg.plan.Name, len(msg.steps))))
		}
		m.sync()

	case vibePlanLoadedMsg:
		if msg.err == nil && msg.plan != nil {
			m.activePlan = msg.plan
			m.planSteps = msg.steps
		}

	case vibePlanStepMsg:
		m.waiting = false
		if msg.err != nil {
			m.push(vibeStyleError.Render(fmt.Sprintf("✗ step %d: %v", msg.step.Ordinal, msg.err)))
			m.push(vibeStyleMuted.Render(fmt.Sprintf(
				"  /plan retry %d   retry this step\n  /plan skip %d    skip and continue\n  /plan replan     regenerate remaining steps",
				msg.step.Ordinal, msg.step.Ordinal)))
		} else if msg.status == planstore.StepStatusCompleted {
			m.push(vibeStyleAI.Render(fmt.Sprintf("⊡ ✓ Step %d done.", msg.step.Ordinal)))
			if msg.result != "" {
				m.push(vibeStyleMuted.Render(msg.result))
			}
		} else {
			m.push(vibeStyleError.Render(fmt.Sprintf("⊡ ✗ Step %d failed.", msg.step.Ordinal)))
			m.push(vibeStyleMuted.Render(fmt.Sprintf(
				"  /plan retry %d   retry this step\n  /plan skip %d    skip and continue\n  /plan replan     regenerate remaining steps",
				msg.step.Ordinal, msg.step.Ordinal)))
		}
		m.sync()
		cmds = append(cmds, m.loadActivePlan())
		// Async auto loop: rather than blocking inside planNext, we dispatch one step
		// at a time through the event loop so the spinner keeps spinning.
		if msg.isAuto && msg.err == nil && msg.status == planstore.StepStatusCompleted {
			m.waiting = true
			cmds = append(cmds, m.planNext(true))
		}
		return m, tea.Batch(cmds...)

	case vibePlanStepUpdateMsg:
		m.waiting = false
		if msg.err != nil {
			m.push(vibeStyleError.Render(fmt.Sprintf("✗ step %d: %v", msg.ordinal, msg.err)))
		} else {
			m.push(vibeStyleTool.Render(fmt.Sprintf("⊡ Step %d → %s", msg.ordinal, msg.status)))
		}
		m.sync()
		return m, m.loadActivePlan()

	case vibePlanListMsg:
		for _, l := range msg.lines {
			m.push(vibeStyleMuted.Render(l))
		}
		m.sync()

	case vibePlanShowStepMsg:
		if msg.err != nil {
			m.push(vibeStyleError.Render("✗ " + msg.err.Error()))
		} else {
			m.push(vibeStyleTool.Render(fmt.Sprintf("── Step %d [%s]: %s", msg.ordinal, msg.status, msg.desc)))
			if msg.result == "" {
				m.push(vibeStyleMuted.Render("  (no output recorded)"))
			} else {
				for _, line := range strings.Split(msg.result, "\n") {
					m.push(vibeStyleMuted.Render("  " + line))
				}
			}
		}
		m.sync()

	case vibePlanReplanMsg:
		m.waiting = false
		if msg.err != nil {
			m.push(vibeStyleError.Render("✗ replan: " + msg.err.Error()))
		} else {
			m.planSteps = msg.steps
			m.push(vibeStyleTool.Render(fmt.Sprintf("⊡ Replanned — %d new steps.", len(msg.steps))))
		}
		m.sync()
		return m, m.loadActivePlan()

	case vibeRunMsg:
		m.waiting = false
		if msg.err != nil {
			m.push(vibeStyleMuted.Render("↻ "+msg.chainID) + "\n" + vibeStyleError.Render("✗ "+msg.err.Error()))
		} else {
			m.push(vibeStyleMuted.Render("↻ "+msg.chainID) + "\n" + vibeStyleAI.Render(fmt.Sprintf("%v", msg.output)))
		}
		m.sync()

	case vibeMCPMsg:
		m.mcpWorkers = msg.workers

	case vibeCobraOutputMsg:
		m.waiting = false
		style := vibeStyleMuted
		if msg.err != nil {
			style = vibeStyleError
		}
		for _, l := range msg.lines {
			if l != "" {
				m.push(style.Render(l))
			}
		}
		m.sync()
		// Reload plan state in case the cobra command changed it.
		return m, m.loadActivePlan()
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)
	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// ── dispatch ──────────────────────────────────────────────────────────────────

func (m vibeModel) dispatch(raw string) (tea.Model, tea.Cmd) {
	m.push(vibeStyleUser.Render("›") + " " + raw)
	m.sync()

	if strings.HasPrefix(raw, "$") {
		m.waiting = true
		return m, m.runShell(strings.TrimSpace(raw[1:]))
	}
	if strings.HasPrefix(raw, "/") {
		return m.slash(raw)
	}
	m.waiting = true
	return m, m.chat(raw)
}

func (m vibeModel) slash(raw string) (tea.Model, tea.Cmd) {
	parts := shellSplit(raw)
	if len(parts) == 0 {
		return m, nil
	}

	switch {
	// ── /plan new <goal> ───────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 2 && parts[1] == "new":
		goal := strings.Join(parts[2:], " ")
		if goal == "" {
			m.push(vibeStyleError.Render("usage: /plan new <goal>"))
			m.sync()
			return m, nil
		}
		if m.plannerChain == nil {
			m.push(vibeStyleError.Render("no planner chain — run: contenox init"))
			m.sync()
			return m, nil
		}
		m.waiting = true
		return m, m.planNew(goal)

	// ── /plan next [--auto] ────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 2 && parts[1] == "next":
		if m.activePlan == nil || m.executorChain == nil {
			m.push(vibeStyleError.Render("no active plan — use /plan new <goal>"))
			m.sync()
			return m, nil
		}
		auto := len(parts) >= 3 && parts[2] == "--auto"
		m.waiting = true
		return m, m.planNext(auto)

	// ── /plan show ─────────────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 2 && parts[1] == "show":
		m.showPlanInline()
		m.sync()
		return m, nil

	// ── /plan list ─────────────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 2 && parts[1] == "list":
		return m, m.planList()

	// ── /plan step <N> ─────────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 3 && parts[1] == "step":
		n, err := strconv.Atoi(parts[2])
		if err != nil {
			m.push(vibeStyleError.Render("usage: /plan step <ordinal>"))
			m.sync()
			return m, nil
		}
		if m.activePlan == nil {
			m.push(vibeStyleError.Render("no active plan"))
			m.sync()
			return m, nil
		}
		return m, m.planShowStep(n)

	// ── /plan retry <N> ────────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 3 && parts[1] == "retry":
		n, err := strconv.Atoi(parts[2])
		if err != nil {
			m.push(vibeStyleError.Render("usage: /plan retry <ordinal>"))
			m.sync()
			return m, nil
		}
		if m.activePlan == nil {
			m.push(vibeStyleError.Render("no active plan"))
			m.sync()
			return m, nil
		}
		return m, m.planUpdateStep(n, planstore.StepStatusPending, "")

	// ── /plan skip <N> ─────────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 3 && parts[1] == "skip":
		n, err := strconv.Atoi(parts[2])
		if err != nil {
			m.push(vibeStyleError.Render("usage: /plan skip <ordinal>"))
			m.sync()
			return m, nil
		}
		if m.activePlan == nil {
			m.push(vibeStyleError.Render("no active plan"))
			m.sync()
			return m, nil
		}
		return m, m.planUpdateStep(n, planstore.StepStatusSkipped, "Skipped by user")

	// ── /plan replan ───────────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 2 && parts[1] == "replan":
		if m.activePlan == nil || m.plannerChain == nil {
			m.push(vibeStyleError.Render("no active plan or planner chain"))
			m.sync()
			return m, nil
		}
		m.waiting = true
		return m, m.planReplan()

	// ── /run --chain <file> [input] ────────────────────────────────────────────
	case parts[0] == "/run":
		return m, m.statelessRun(parts[1:])

	// ── /session ──────────────────────────────────────────────────────────────
	case parts[0] == "/session":
		if len(parts) >= 2 {
			switch parts[1] {
			case "show":
				m.push(vibeStyleMuted.Render(fmt.Sprintf("session│ %.8s  messages: %d", m.sessionID, len(m.history.Messages))))
				m.sync()
				return m, nil
			case "list":
				return m, m.runCobraCmd(sessionListCmd, nil)
			case "new":
				if len(parts) >= 3 {
					return m, m.runCobraCmd(sessionNewCmd, parts[2:3])
				}
				return m, m.runCobraCmd(sessionNewCmd, nil)
			case "switch":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /session switch <name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(sessionSwitchCmd, parts[2:3])
			case "delete":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /session delete <name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(sessionDeleteCmd, parts[2:3])
			}
		}
		m.push(vibeStyleError.Render("usage: /session list|new|switch|delete|show"))
		m.push(vibeStyleMuted.Render("  /session list             list all sessions (* = active)"))
		m.push(vibeStyleMuted.Render("  /session new [name]       create a new session"))
		m.push(vibeStyleMuted.Render("  /session switch <name>    switch active session"))
		m.push(vibeStyleMuted.Render("  /session delete <name>    delete a session"))
		m.push(vibeStyleMuted.Render("  /session show             show session ID and message count"))
		m.sync()
		return m, nil

	// ── /clear ───────────────────────────────────────────────────────────────────
	case parts[0] == "/clear":
		m.log = nil
		m.sync()
		return m, nil

	// ── /help ────────────────────────────────────────────────────────────────────
	case parts[0] == "/help":
		m.push(vibeStyleMuted.Render(strings.TrimSpace(`
  <text>                          chat (same session as contenox chat)
  $ <cmd>                         shell → stdout injected into LLM context
  /clear                          clear the viewport log
  /plan new <goal>                generate a new plan
  /plan next [--auto]             execute next step (--auto loops until done or failed)
  /plan show                      print active plan to log
  /plan list                      list all plans (* = active)
  /plan step <N>                  show the output of step N
  /plan retry <N>                 reset step N to pending
  /plan skip  <N>                 mark step N as skipped
  /plan replan                    regenerate remaining steps with LLM
  /plan delete <name>             delete a plan by name
  /plan clean                     delete all completed/archived plans
  /run --chain <file> [input]     run a chain statelessly (like contenox run)
  /model list|add|remove          manage declared models
  /model set-context <name> --context <len>   set context window (e.g. 128k)
  /config get|set|list <key>      read/write persistent config
  /session list|new|switch|delete manage chat sessions
  /session show                   show session ID and message count
  /backend list|show|add|remove   manage LLM backends
  /hook list|show|add|remove|update   manage remote hooks
  /mcp list|show|add|remove|update    manage MCP servers
  /mcp                            refresh MCP workers in sidebar
  Ctrl+B                          cycle layout: normal → wide → full → …
  Ctrl+C                          quit`)))
		m.sync()
		return m, nil

	// ── /model ──────────────────────────────────────────────────────────────────
	case parts[0] == "/model":
		if len(parts) >= 2 {
			switch parts[1] {
			case "list", "ls":
				_ = modelListCmd.Flags().Set("declared", "false")
				return m, m.runCobraCmd(modelListCmd, nil)
			case "add":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /model add <model-name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(modelAddCmd, parts[2:3])
			case "remove", "rm":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /model remove <model-name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(modelRemoveCmd, parts[2:3])
			case "set-context":
				// /model set-context <name> --context <len>
				if len(parts) < 4 {
					m.push(vibeStyleError.Render("usage: /model set-context <model-name> --context <len>  (e.g. 128k)"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(modelSetContextCmd, parts[2:])
			}
		}
		m.push(vibeStyleError.Render("usage: /model list|add|remove|set-context"))
		m.sync()
		return m, nil

	// ── /config get|set|list ────────────────────────────────────────────────────
	case parts[0] == "/config":
		if len(parts) >= 2 {
			switch parts[1] {
			case "get":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /config get <key>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(configGetCmd, parts[2:3])
			case "set":
				if len(parts) < 4 {
					m.push(vibeStyleError.Render("usage: /config set <key> <value>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(configSetCmd, parts[2:4])
			case "list", "ls":
				return m, m.runCobraCmd(configListCmd, nil)
			}
		}
		m.push(vibeStyleError.Render("usage: /config get|set|list"))
		m.sync()
		return m, nil

	// ── /plan delete <name> ─────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 3 && parts[1] == "delete":
		return m, m.runCobraCmd(planDeleteCmd, parts[2:3])

	// ── /plan clean ─────────────────────────────────────────────────────────────
	case parts[0] == "/plan" && len(parts) >= 2 && parts[1] == "clean":
		return m, m.runCobraCmd(planCleanCmd, nil)

	case parts[0] == "/plan":
		m.push(vibeStyleError.Render("unknown /plan subcommand. usage:"))
		m.push(vibeStyleMuted.Render("  /plan new <goal>   generate a new plan"))
		m.push(vibeStyleMuted.Render("  /plan next         execute next pending step"))
		m.push(vibeStyleMuted.Render("  /plan next --auto  loop until done or failed"))
		m.push(vibeStyleMuted.Render("  /plan show         print active plan"))
		m.push(vibeStyleMuted.Render("  /plan list         list all plans"))
		m.push(vibeStyleMuted.Render("  /plan step <N>     show output of step N"))
		m.push(vibeStyleMuted.Render("  /plan retry <N>    reset step N to pending"))
		m.push(vibeStyleMuted.Render("  /plan skip  <N>    mark step N as skipped"))
		m.push(vibeStyleMuted.Render("  /plan replan       regenerate remaining steps"))
		m.push(vibeStyleMuted.Render("  /plan delete <n>   delete a plan by name"))
		m.push(vibeStyleMuted.Render("  /plan clean        delete all completed plans"))
		m.sync()
		return m, nil

	// ── /backend ─────────────────────────────────────────────────────────────────
	case parts[0] == "/backend":
		if len(parts) >= 2 {
			switch parts[1] {
			case "list":
				return m, m.runCobraCmd(backendListCmd, nil)
			case "show":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /backend show <name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(backendShowCmd, parts[2:3])
			case "add":
				// /backend add <name> --type <type> [--url <url>] [--api-key-env <env>]
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /backend add <name> --type <olama|openai|gemini|vllm> [--url <url>] [--api-key-env <env>]"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(backendAddCmd, parts[2:])
			case "remove", "rm":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /backend remove <name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(backendRemoveCmd, parts[2:3])
			}
		}
		m.push(vibeStyleError.Render("usage: /backend list|show|add|remove"))
		m.sync()
		return m, nil

	// ── /hook ─────────────────────────────────────────────────────────────────────
	case parts[0] == "/hook":
		if len(parts) >= 2 {
			switch parts[1] {
			case "list":
				return m, m.runCobraCmd(hookListCmd, nil)
			case "show":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /hook show <name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(hookShowCmd, parts[2:3])
			case "add":
				// /hook add <name> --url <url> [--timeout <ms>] [--header <h>] [--inject <k=v>]
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /hook add <name> --url <url> [--timeout <ms>]"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(hookAddCmd, parts[2:])
			case "remove", "rm":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /hook remove <name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(hookRemoveCmd, parts[2:3])
			case "update":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /hook update <name> [--timeout <ms>] [--header <h>]"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(hookUpdateCmd, parts[2:])
			}
		}
		m.push(vibeStyleError.Render("usage: /hook list|show|add|remove|update"))
		m.sync()
		return m, nil

	// ── /mcp ─────────────────────────────────────────────────────────────────────
	case parts[0] == "/mcp":
		if len(parts) >= 2 {
			switch parts[1] {
			case "list":
				return m, m.runCobraCmd(mcpListCmd, nil)
			case "show":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /mcp show <name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(mcpShowCmd, parts[2:3])
			case "add":
				// /mcp add <name> --transport <type> [--url <url>] [--command <cmd>] [--args <a,b>]
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /mcp add <name> --transport <stdio|sse|http> [--url <url>] [--command <cmd>]"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(mcpAddCmd, parts[2:])
			case "remove", "rm":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /mcp remove <name>"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(mcpRemoveCmd, parts[2:3])
			case "update":
				if len(parts) < 3 {
					m.push(vibeStyleError.Render("usage: /mcp update <name> [--inject <k=v>] [--header <h>]"))
					m.sync()
					return m, nil
				}
				return m, m.runCobraCmd(mcpUpdateCmd, parts[2:])
			}
		}
		// bare /mcp = refresh sidebar
		return m, m.refreshMCP()

	default:
		m.push(vibeStyleError.Render("unknown command: " + parts[0] + "  (try /help)"))
		m.sync()
		return m, nil
	}
}

// ── tea.Cmd factories ─────────────────────────────────────────────────────────

func (m vibeModel) chat(input string) tea.Cmd {
	return func() tea.Msg {
		userMsg := taskengine.Message{
			Role:      "user",
			Content:   input,
			Timestamp: time.Now().UTC(),
		}
		chatIn := taskengine.ChatHistory{Messages: append(m.history.Messages, userMsg)}
		execCtx := libtracker.WithNewRequestID(taskengine.WithTemplateVars(m.ctx, map[string]string{
			"model": m.model, "provider": m.provider, "chain": m.chatChain.ID,
		}))
		out, _, _, err := m.engine.TaskService.Execute(execCtx, m.chatChain, chatIn, taskengine.DataTypeChatHistory)
		if err != nil {
			return vibeLLMMsg{err: err}
		}
		updated, ok := out.(taskengine.ChatHistory)
		if !ok {
			return vibeLLMMsg{err: fmt.Errorf("unexpected output type: %T", out)}
		}
		// Persist diff (same as chat_cmd.go).
		if m.sessionID != "" {
			chatMgr := chatservice.NewManager(nil)
			_ = withTransaction(m.ctx, m.db, func(tx libdb.Exec) error {
				return chatMgr.PersistDiff(m.ctx, tx, m.sessionID, updated.Messages)
			})
		}
		return vibeLLMMsg{history: updated}
	}
}

func (m vibeModel) runShell(cmd string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, shellTimeout)
		defer cancel()
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		stdout, err := c.Output()
		stderr := ""
		if e, ok := err.(*exec.ExitError); ok {
			stderr = string(e.Stderr)
		}
		return vibeShellMsg{
			cmd:    cmd,
			stdout: strings.TrimSpace(string(stdout)),
			stderr: stderr,
			err:    err,
		}
	}
}

func (m vibeModel) planNew(goal string) tea.Cmd {
	return func() tea.Msg {
		execCtx := libtracker.WithNewRequestID(taskengine.WithTemplateVars(m.ctx, map[string]string{
			"model": m.model, "provider": m.provider, "chain": m.plannerChain.ID,
		}))

		// Delegate to planservice so we don't reinvent DB writes + markdown sync.
		planSvc := buildPlanService(m.db, m.engine, m.contenoxDir)
		plan, steps, _, err := planSvc.New(execCtx, goal, m.plannerChain)
		if err != nil {
			return vibePlanCreatedMsg{err: err}
		}

		// Update the KV pointer so runPlanList marks the right plan as active.
		_ = withTransaction(m.ctx, m.db, func(tx libdb.Exec) error {
			return setActivePlanID(m.ctx, tx, plan.ID)
		})

		return vibePlanCreatedMsg{plan: plan, steps: steps}
	}
}

// planNext executes exactly one pending step and returns. When auto=true the
// isAuto flag is set on the returned message so Update() can queue the next step,
// giving the event loop a chance to redraw between steps (spinner stays alive).
func (m vibeModel) planNext(auto bool) tea.Cmd {
	return func() tea.Msg {
		execCtx := libtracker.WithNewRequestID(taskengine.WithTemplateVars(m.ctx, map[string]string{
			"model": m.model, "provider": m.provider, "chain": m.executorChain.ID,
		}))
		var next *planstore.PlanStep
		for _, s := range m.planSteps {
			if s.Status == planstore.StepStatusPending {
				next = s
				break
			}
		}
		if next == nil {
			return vibePlanStepMsg{err: fmt.Errorf("no pending steps"), isAuto: auto}
		}

		// Delegate execution to planservice — it handles DB updates + markdown sync.
		planSvc := buildPlanService(m.db, m.engine, m.contenoxDir)
		args := planservice.Args{WithShell: true, WithAuto: auto}
		result, _, execErr := planSvc.Next(execCtx, args, m.executorChain)

		// Reload the step to get its updated status from the database.
		var status planstore.StepStatus
		if execErr != nil {
			status = planstore.StepStatusFailed
		} else {
			status = planstore.StepStatusCompleted
		}
		return vibePlanStepMsg{step: next, status: status, result: result, err: execErr, isAuto: auto}
	}
}

// vibePlanShowStepMsg carries the result for /plan step <N>.
type vibePlanShowStepMsg struct {
	ordinal int
	desc    string
	status  string
	result  string
	err     error
}

func (m vibeModel) planShowStep(ordinal int) tea.Cmd {
	return func() tea.Msg {
		exec := m.db.WithoutTransaction()
		activeID, err := getActivePlanID(m.ctx, exec)
		if err != nil || activeID == "" {
			return vibePlanShowStepMsg{ordinal: ordinal, err: fmt.Errorf("no active plan")}
		}
		store := planstore.New(exec)
		steps, err := store.ListPlanSteps(m.ctx, activeID)
		if err != nil {
			return vibePlanShowStepMsg{ordinal: ordinal, err: err}
		}
		for _, s := range steps {
			if s.Ordinal == ordinal {
				return vibePlanShowStepMsg{
					ordinal: s.Ordinal,
					desc:    s.Description,
					status:  string(s.Status),
					result:  s.ExecutionResult,
				}
			}
		}
		return vibePlanShowStepMsg{ordinal: ordinal, err: fmt.Errorf("step %d not found", ordinal)}
	}
}

func (m vibeModel) planUpdateStep(ordinal int, newStatus planstore.StepStatus, reason string) tea.Cmd {
	return func() tea.Msg {
		exec := m.db.WithoutTransaction()
		activeID, err := getActivePlanID(m.ctx, exec)
		if err != nil || activeID == "" {
			return vibePlanStepUpdateMsg{ordinal: ordinal, err: fmt.Errorf("no active plan")}
		}
		store := planstore.New(exec)
		steps, err := store.ListPlanSteps(m.ctx, activeID)
		if err != nil {
			return vibePlanStepUpdateMsg{ordinal: ordinal, err: err}
		}
		var target *planstore.PlanStep
		for _, s := range steps {
			if s.Ordinal == ordinal {
				target = s
				break
			}
		}
		if target == nil {
			return vibePlanStepUpdateMsg{ordinal: ordinal, err: fmt.Errorf("step %d not found", ordinal)}
		}
		if err := store.UpdatePlanStepStatus(m.ctx, target.ID, newStatus, reason); err != nil {
			return vibePlanStepUpdateMsg{ordinal: ordinal, err: err}
		}
		_ = syncPlanMarkdown(m.ctx, exec, activeID, m.contenoxDir)
		return vibePlanStepUpdateMsg{ordinal: ordinal, status: newStatus}
	}
}

func (m vibeModel) planList() tea.Cmd {
	return func() tea.Msg {
		exec := m.db.WithoutTransaction()
		activeID, _ := getActivePlanID(m.ctx, exec)
		store := planstore.New(exec)
		plans, err := store.ListPlans(m.ctx)
		if err != nil {
			return vibePlanListMsg{lines: []string{vibeStyleError.Render("✗ " + err.Error())}}
		}
		if len(plans) == 0 {
			return vibePlanListMsg{lines: []string{vibeStyleMuted.Render("no plans yet — use /plan new <goal>")}}
		}
		var lines []string
		for _, p := range plans {
			prefix := "  "
			if p.ID == activeID {
				prefix = "* "
			}
			steps, _ := store.ListPlanSteps(m.ctx, p.ID)
			completed := 0
			for _, s := range steps {
				if s.Status == planstore.StepStatusCompleted {
					completed++
				}
			}
			lines = append(lines, fmt.Sprintf("%s%-20s [%d/%d] %s", prefix, p.Name, completed, len(steps), p.Status))
		}
		return vibePlanListMsg{lines: lines}
	}
}

func (m vibeModel) planReplan() tea.Cmd {
	return func() tea.Msg {
		execCtx := libtracker.WithNewRequestID(taskengine.WithTemplateVars(m.ctx, map[string]string{
			"model": m.model, "provider": m.provider, "chain": m.plannerChain.ID,
		}))
		// Delegate entirely to planservice — it loads the active plan, deletes
		// pending steps, calls the planner chain, and saves the new steps.
		planSvc := buildPlanService(m.db, m.engine, m.contenoxDir)
		newSteps, _, err := planSvc.Replan(execCtx, m.plannerChain)
		if err != nil {
			return vibePlanReplanMsg{err: fmt.Errorf("replan failed: %w", err)}
		}
		return vibePlanReplanMsg{steps: newSteps}
	}
}

func (m vibeModel) loadActivePlan() tea.Cmd {
	return func() tea.Msg {
		exec := m.db.WithoutTransaction()
		id, err := getActivePlanID(m.ctx, exec)
		if err != nil || id == "" {
			return vibePlanLoadedMsg{}
		}
		store := planstore.New(exec)
		plan, err := store.GetPlanByID(m.ctx, id)
		if err != nil {
			return vibePlanLoadedMsg{err: err}
		}
		steps, err := store.ListPlanSteps(m.ctx, id)
		if err != nil {
			return vibePlanLoadedMsg{err: err}
		}
		return vibePlanLoadedMsg{plan: plan, steps: steps}
	}
}

func (m vibeModel) statelessRun(args []string) tea.Cmd {
	chainPath, inputStr := "", ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--chain" && i+1 < len(args) {
			chainPath = args[i+1]
			i++
		} else {
			inputStr += args[i] + " "
		}
	}
	inputStr = strings.TrimSpace(inputStr)
	if chainPath == "" {
		m.push(vibeStyleError.Render("usage: /run --chain <path> [input]"))
		m.sync()
		return func() tea.Msg { return nil }
	}
	if !filepath.IsAbs(chainPath) {
		chainPath = filepath.Join(m.contenoxDir, chainPath)
	}
	chainData, err := os.ReadFile(chainPath)
	if err != nil {
		m.push(vibeStyleError.Render("✗ " + err.Error()))
		m.sync()
		return func() tea.Msg { return nil }
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(chainData, &chain); err != nil {
		m.push(vibeStyleError.Render("✗ invalid chain JSON: " + err.Error()))
		m.sync()
		return func() tea.Msg { return nil }
	}
	input := inputStr
	return func() tea.Msg {
		execCtx := libtracker.WithNewRequestID(taskengine.WithTemplateVars(m.ctx, map[string]string{
			"model": m.model, "provider": m.provider, "chain": chain.ID,
		}))
		out, _, _, err := m.engine.TaskService.Execute(execCtx, &chain, input, taskengine.DataTypeString)
		return vibeRunMsg{chainID: chain.ID, output: out, err: err}
	}
}

func (m vibeModel) refreshMCP() tea.Cmd {
	return func() tea.Msg {
		var workers []string
		if m.engine != nil && m.engine.MCPManager != nil {
			workers = m.engine.MCPManager.ActiveWorkers()
		}
		return vibeMCPMsg{workers: workers}
	}
}

// vibeCobraOutputMsg is fired when a runCobraCmd completes.
type vibeCobraOutputMsg struct {
	lines []string
	err   error
}

// resetCobraFlags resets all flags on cmd to their default values and clears
// the Changed bit. This is necessary because global cobra commands are reused
// across TUI slash-command invocations — without a reset, flag state from a
// prior call bleeds into subsequent calls.
func resetCobraFlags(cmd *cobra.Command) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
}

// runCobraCmd executes a cobra RunE handler with output redirected to a buffer.
// Each line of output (stdout + stderr merged) is pushed into the TUI log.
// The given cobraCmd MUST have its --db persistent flag set correctly so
// openPlanDB / openBackendDB resolve the right database.
func (m vibeModel) runCobraCmd(cobraCmd *cobra.Command, args []string) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		cobraCmd.SetOut(&buf)
		cobraCmd.SetErr(&buf)
		// Bug F fix: reset flag state from any previous TUI invocation of this
		// same cobra command before parsing new args.
		resetCobraFlags(cobraCmd)
		// ParseFlags must be called before RunE so that flag values are populated.
		// Calling RunE directly (without going through cmd.Execute) skips Cobra's
		// normal flag-parsing path.
		if err := cobraCmd.ParseFlags(args); err != nil {
			return vibeCobraOutputMsg{err: err}
		}
		err := cobraCmd.RunE(cobraCmd, cobraCmd.Flags().Args())
		if err != nil {
			buf.WriteString("\n✗ " + err.Error())
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			lines = append(lines, l)
		}
		return vibeCobraOutputMsg{lines: lines, err: err}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m vibeModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading…"
	}
	header := m.renderHeader()

	centerBlock := lipgloss.NewStyle().
		Width(m.viewport.Width).
		Height(m.viewport.Height).
		Render(m.viewport.View())

	promptBlock := lipgloss.NewStyle().
		Width(m.viewport.Width).
		Height(4).
		PaddingTop(1).
		Render(m.renderPrompt(m.viewport.Width))

	chatArea := lipgloss.JoinVertical(lipgloss.Left, centerBlock, promptBlock)

	var body string
	if m.sidebarW > 0 {
		sidebar := vibeClipLines(m.renderSidebar(), m.sidebarMaxH)
		sidebarBox := vibeStyleBorderBox.
			Width(m.sidebarW).
			Height(m.sidebarMaxH).
			Render(sidebar)
		gap := lipgloss.NewStyle().Width(1).Height(m.sidebarMaxH + 2).Render("")
		body = lipgloss.JoinHorizontal(lipgloss.Top, sidebarBox, gap, chatArea)
	} else {
		body = chatArea
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, body, m.renderStatus())
}

func (m vibeModel) renderHeader() string {
	model := m.model
	if model == "" {
		model = "N/A"
	}
	left := fmt.Sprintf(" contenox │ model: %s │ session: %.8s", model, m.sessionID)
	right := " Enter=send  /help=cmds  ^B=layout  ^C=quit "
	// lipgloss.PlaceHorizontal handles the spacing perfectly and avoids byte-slicing
	// panics that can occur when the terminal is resized mid-multi-byte character.
	content := lipgloss.PlaceHorizontal(m.width-2, lipgloss.Left, left+right)
	if lipgloss.Width(left)+lipgloss.Width(right) <= m.width-2 {
		content = lipgloss.PlaceHorizontal(m.width-2, lipgloss.Left, left+
			strings.Repeat(" ", m.width-2-lipgloss.Width(left)-lipgloss.Width(right))+right)
	}
	return vibeStyleHeader.Width(m.width).Render(content)
}

func (m vibeModel) renderSidebar() string {
	// contentW = usable text columns inside the border box.
	// vibeStyleBorderBox has a rounded border (2 cols) + padding(0,1) (2 cols) = 4 overhead.
	contentW := max(m.sidebarW-4, 4)
	// prefix for a plan step line: "  ○ N. " = 7 cols (single digit), 8 cols (double digit)
	stepPrefixW := 8 // conservative: handles up to step 99

	var sb strings.Builder
	sb.WriteString(vibeStyleSidebarTitle.Render("PLAN") + "\n")
	if m.activePlan != nil {
		// plan name: "  " prefix = 2 cols
		sb.WriteString(vibeStyleMuted.Render("  "+vibeTruncate(m.activePlan.Name, contentW-2)) + "\n")
		// Find the first pending step — that's the one currently executing (when waiting).
		activeStepID := ""
		if m.waiting {
			for _, s := range m.planSteps {
				if s.Status == planstore.StepStatusPending {
					activeStepID = s.ID
					break
				}
			}
		}
		for _, s := range m.planSteps {
			box := "○"
			switch s.Status {
			case planstore.StepStatusCompleted:
				box = "✓"
			case planstore.StepStatusFailed:
				box = "✗"
			case planstore.StepStatusSkipped:
				box = "–"
			default:
				if s.ID == activeStepID {
					box = "⟳"
				}
			}
			descW := contentW - stepPrefixW
			if descW < 1 {
				descW = 1
			}
			line := fmt.Sprintf("  %s %d. %s", box, s.Ordinal, vibeTruncate(s.Description, descW))
			sb.WriteString(vibePlanStepStyle(string(s.Status)).Render(line) + "\n")
		}
	} else {
		sb.WriteString(vibeStyleMuted.Render("  (none — /plan new <goal>)") + "\n")
	}

	// MCP/HOOKS items: "  ● " prefix = 4 cols
	itemPrefixW := 4
	sb.WriteString("\n" + vibeStyleSidebarSection.Render("MCP") + "\n")
	if len(m.mcpWorkers) == 0 {
		sb.WriteString(vibeStyleMuted.Render("  (none)") + "\n")
	}
	for _, w := range m.mcpWorkers {
		sb.WriteString("  " + vibeDot(true) + " " + vibeStyleMuted.Render(vibeTruncate(w, contentW-itemPrefixW)) + "\n")
	}

	sb.WriteString("\n" + vibeStyleSidebarSection.Render("HOOKS") + "\n")
	if m.engine == nil || len(m.engine.LocalHooks) == 0 {
		sb.WriteString(vibeStyleMuted.Render("  (none)") + "\n")
	}
	for _, h := range m.engine.LocalHooks {
		sb.WriteString("  " + vibeDot(true) + " " + vibeStyleMuted.Render(vibeTruncate(h, contentW-itemPrefixW)) + "\n")
	}
	return sb.String()
}

func (m vibeModel) renderPrompt(w int) string {
	prefix := "  "
	if m.waiting {
		prefix = vibeStyleAI.Render(" ⟳ ")
	}
	m.input.SetWidth(w - lipgloss.Width(prefix))
	return lipgloss.JoinHorizontal(lipgloss.Top, vibeStyleInputPrompt.Render(prefix), m.input.View())
}

func (m vibeModel) renderStatus() string {
	if m.waiting {
		msg := " ⟳  working — model is processing your request… "
		return vibeStyleStatusWorking.Width(m.width).Render(msg)
	}
	return vibeStyleStatus.Width(m.width).Render(" ready ")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (m *vibeModel) push(line string) {
	m.log = append(m.log, line)
}

func (m *vibeModel) sync() {
	if m.viewport.Width <= 0 {
		return
	}
	// Word-wrap each log line to the exact viewport width so long LLM responses
	// don't overflow horizontally.
	wrapStyle := lipgloss.NewStyle().Width(m.viewport.Width)
	wrapped := make([]string, 0, len(m.log))
	for _, line := range m.log {
		wrapped = append(wrapped, wrapStyle.Render(line))
	}
	m.viewport.SetContent(strings.Join(wrapped, "\n"))
	m.viewport.GotoBottom()
}

func (m vibeModel) relayout() vibeModel {
	const minTerminalForSidebar = 100
	const minSidebar = 28
	const maxSidebar = 54

	switch {
	case m.width < minTerminalForSidebar:
		// Terminal too narrow — always full, ignore layout preference.
		m.sidebarW = 0
	case m.layout == layoutFull:
		m.sidebarW = 0
	case m.layout == layoutWide:
		m.sidebarW = maxSidebar
	default: // layoutNormal
		w := m.width / 5
		if w < minSidebar {
			w = minSidebar
		} else if w > maxSidebar {
			w = maxSidebar
		}
		m.sidebarW = w
	}

	// promptH = PaddingTop(1) + textarea SetHeight(3)
	const promptH = 4
	// bodyH = rows available between header and status bar
	bodyH := m.height - 2

	var centerW int
	if m.sidebarW > 0 {
		// sidebarW outer cols (m.sidebarW + 2) + 1 (gap)
		centerW = m.width - (m.sidebarW + 2) - 1
	} else {
		centerW = m.width
	}
	if centerW < 10 {
		centerW = 10
	}
	m.viewport.Width = centerW
	// viewport must fill exactly bodyH minus the prompt so center+prompt = bodyH
	m.viewport.Height = bodyH - promptH
	if m.viewport.Height < 5 {
		m.viewport.Height = 5
	}
	// sidebarMaxH: content rows inside the border box.
	// sidebar outer = sidebarMaxH + 2 (border); must fill full body height.
	m.sidebarMaxH = bodyH - 2
	if m.sidebarMaxH < 4 {
		m.sidebarMaxH = 4
	}
	m.input.SetWidth(centerW - lipgloss.Width("  ") - 1)
	return m
}

// vibeClipLines truncates s to at most n non-empty-terminal lines so the sidebar
// can never overflow the available body height and push the header/status off screen.
func vibeClipLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// vibeTruncate safely truncates s to n rune positions (not bytes), preventing
// panics on multi-byte characters such as emoji or non-ASCII text.
func vibeTruncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}

// showPlanInline prints the active plan into the log without a tea.Cmd round-trip.
func (m *vibeModel) showPlanInline() {
	if m.activePlan == nil {
		m.push(vibeStyleError.Render("no active plan — use /plan new <goal>"))
		return
	}
	m.push(vibeStyleTool.Render(fmt.Sprintf("⊡ %s — %d steps", m.activePlan.Name, len(m.planSteps))))
	for _, s := range m.planSteps {
		box := "[ ]"
		switch s.Status {
		case planstore.StepStatusCompleted:
			box = "[x]"
		case planstore.StepStatusFailed, planstore.StepStatusSkipped:
			box = "[-]"
		}
		m.push(vibeStyleMuted.Render(fmt.Sprintf("  %d. %s %s", s.Ordinal, box, s.Description)))
	}
}

// shellSplit splits a string into tokens respecting single and double quoted spans.
// Unlike strings.Fields it does not break on spaces inside quotes, so slash
// commands like /run --chain "my file.json" parse correctly.
func shellSplit(s string) []string {
	var tokens []string
	var cur strings.Builder
	inQ := rune(0)
	for _, r := range s {
		switch {
		case inQ != 0 && r == inQ:
			inQ = 0 // close quote
		case inQ == 0 && (r == '"' || r == '\''):
			inQ = r // open quote
		case inQ == 0 && r == ' ':
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
