package acpsvc

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/contenox/beam/internal/kernel/reasoning"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelcapability"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
)

// allACPCommands is the full, capability-unfiltered admin command set — every
// command this package knows how to dispatch, regardless of what the current
// transport can actually run. It exists so parseCommand's recognition (via
// acpCommandNames, below) is never narrower than what (*Transport).acpCommands
// advertises: a command that got dropped from the advertised menu must still be
// recognized as a command when a client sends it anyway (see handleMission's
// teaching error for /mission), not silently forwarded as ordinary prompt text.
// Use (*Transport).acpCommands for anything that reaches a client — the menu,
// /help, session/new's available_commands — never this directly.
func allACPCommands() []libacp.AvailableCommand {
	return []libacp.AvailableCommand{
		{Name: "help", Description: "List the available commands."},
		{Name: "doctor", Description: "Check provider/model/backend readiness (read-only — no test prompt is sent)."},
		{Name: "clear", Description: "Clear this session's conversation history."},
		{Name: "compact", Description: "Summarize older history into a single message to reclaim context.", Input: &libacp.AvailableCommandInput{Hint: "[keep]"}},
		{Name: "rename", Description: "Show or set this session's title: /rename <title> (- resets it).", Input: &libacp.AvailableCommandInput{Hint: "[title|-]"}},
		{Name: "model", Description: "Show the current model, or set it: /model <name>.", Input: &libacp.AvailableCommandInput{Hint: "[model-name]"}},
		{Name: "provider", Description: "Show the current provider, or set it: /provider <name>.", Input: &libacp.AvailableCommandInput{Hint: "[provider-name]"}},
		{Name: "max-tokens", Description: "Show or set the default response token cap: /max-tokens <count>.", Input: &libacp.AvailableCommandInput{Hint: "[count]"}},
		{Name: "think", Description: "Show or set this session's reasoning level: /think <level|off|auto>.", Input: &libacp.AvailableCommandInput{Hint: "[level|off|auto]"}},
		{Name: "capability", Description: "Show or set persistent provider/model capability overrides.", Input: &libacp.AvailableCommandInput{Hint: "set|show|unset <provider> <model> [--think true|false]"}},
		{Name: "policy", Description: "Show the active HITL policy, or switch it: /policy <name>.", Input: &libacp.AvailableCommandInput{Hint: "[policy-name]"}},
		{Name: "mission", Description: "Fire a mission from this session: /mission [agent-name] <intent>.", Input: &libacp.AvailableCommandInput{Hint: "[agent-name] <intent>"}},
	}
}

// acpCommands is the admin command set actually advertised to THIS transport's
// ACP clients — capability-filtered from allACPCommands. /mission is dropped
// unless hasMissionCapability reports the fleet kernel and agent resolver it
// needs are both wired (only true in a serve-hosted session): ACP is
// advertise-what-works, so a standalone `contenox acp` process (what Zed
// spawns, with no fleet kernel of its own) must never list a command that can
// only error out. The protocol only uses this list as a client-side
// autocomplete/menu hint — an invoked command arrives back as ordinary prompt
// text, which Prompt intercepts.
func (t *Transport) acpCommands() []libacp.AvailableCommand {
	all := allACPCommands()
	if t.hasMissionCapability() {
		return all
	}
	out := make([]libacp.AvailableCommand, 0, len(all)-1)
	for _, c := range all {
		if c.Name == "mission" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// acpCommandNames is the set of recognized command names, used by parseCommand.
// Built from allACPCommands (the unfiltered superset), not the per-transport
// advertised list — see allACPCommands' doc comment for why.
var acpCommandNames = func() map[string]struct{} {
	m := make(map[string]struct{}, len(allACPCommands()))
	for _, c := range allACPCommands() {
		m[c.Name] = struct{}{}
	}
	return m
}()

// parseCommand recognizes a leading slash command whose first token is one of
// the advertised admin commands. It matches the first token ONLY when it leads
// the input, so a pasted path ("/home/user/x") or text that merely mentions a
// slash ("what does /etc/passwd do") is left as a normal prompt.
func parseCommand(input string) (name, args string, ok bool) {
	s := strings.TrimSpace(input)
	if !strings.HasPrefix(s, "/") {
		return "", "", false
	}
	rest := s[1:]
	first := rest
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		first = rest[:i]
		args = strings.TrimSpace(rest[i+1:])
	}
	if _, known := acpCommandNames[first]; !known {
		return "", "", false
	}
	return first, args, true
}

// commandShapeRE matches the token AFTER the leading slash of an input that is
// SHAPED like one of this server's slash commands: lowercase letters, digits
// and dashes only, which is the shape every name in allACPCommands has.
//
// It is deliberately much narrower than parseCommand's "everything up to the
// first space", because it is what decides whether an UNRECOGNIZED leading
// slash is answered locally or handed to the model. The three shapes that
// legitimately begin with a slash all fall outside the class and keep reaching
// the model untouched: an absolute path carries a second slash ("/etc/passwd",
// "/home/x y"), a capitalized or extension-bearing path carries characters
// outside it ("/Users/foo", "/tmp/a.txt"), and prose that merely MENTIONS a
// path never leads with one ("what does /etc do").
var commandShapeRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// unknownCommandName reports the name of an input that LOOKS like one of this
// server's slash commands but names none of them — "/totallyfakecommand" —
// together with true. It is the second half of the dispatch decision
// parseCommand opens: parseCommand answers "is this a command I can run", this
// answers "is this a command at all".
//
// Without it an unrecognized slash line fell through as ordinary prompt text:
// the model spent a real turn improvising an error message, which cost tokens,
// took seconds, and was different every time — a non-deterministic answer to a
// question the server can answer exactly. See answerUnknownCommand.
func unknownCommandName(input string) (string, bool) {
	s := strings.TrimSpace(input)
	if !strings.HasPrefix(s, "/") {
		return "", false
	}
	first := s[1:]
	if i := strings.IndexFunc(first, unicode.IsSpace); i >= 0 {
		first = first[:i]
	}
	if !commandShapeRE.MatchString(first) {
		return "", false
	}
	if _, known := acpCommandNames[first]; known {
		return "", false
	}
	return first, true
}

// unknownCommandMessage is the teaching line an unknown command is answered
// with. It carries the "⚠️  " prefix every other command failure carries (see
// dispatchCommand's error path) so the operator reads one refusal vocabulary,
// and it names the ONE next action rather than dumping the whole menu.
func unknownCommandMessage(name string) string {
	return fmt.Sprintf("⚠️  unknown command: /%s — /help lists commands", name)
}

// answerUnknownCommand ends the turn locally with the teaching line: one
// agent_message_chunk, StopReasonEndTurn, and NO model call at all.
//
// It deliberately does NOT persistCommandTurn, unlike dispatchCommand's error
// path, which persists a KNOWN command that ran and failed. The distinction is
// what actually happened: `/model bogus` is an act whose failure explains why
// the model did not change, so the transcript owes the operator that record;
// `/totallyfakecommand` never entered dispatch, changed nothing, and cost
// nothing. Writing it would put a typo in the durable transcript — and, when it
// is a session's FIRST message, make that typo the session's derived title
// (see firstUserMessageTitle) for the rest of its life. This is the same
// "the transcript is not the right home for this" exception
// commandRewritesHistory carves out, decided one step earlier.
func (t *Transport) answerUnknownCommand(ctx context.Context, sid libacp.SessionID, name string) libacp.PromptResponse {
	t.sendUpdate(ctx, libacp.SessionNotification{
		SessionID: sid,
		Update:    libacp.NewAgentMessageChunk(unknownCommandMessage(name)),
	})
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}
}

// dispatchCommand runs an admin command and reports the outcome to the client
// as an agent message. Command failures are surfaced inline (not as a protocol
// error) and still end the turn, so the editor shows them in the conversation.
func (t *Transport) dispatchCommand(ctx context.Context, sid libacp.SessionID, sess *sessionEntry, name, args string) (libacp.PromptResponse, error) {
	reportErr, _, end := t.tracker().Start(ctx, "command", "acp_session", "session_id", string(sid), "command", name)
	defer end()

	var (
		out string
		err error
	)
	switch name {
	case "help":
		out = t.handleHelp()
	case "doctor":
		out, err = t.handleDoctor(ctx)
	case "model":
		out, err = t.handleModel(ctx, args)
	case "provider":
		out, err = t.handleProvider(ctx, args)
	case "max-tokens":
		out, err = t.handleMaxTokens(ctx, args)
	case "think":
		out, err = t.handleThink(sess, args)
	case "capability":
		out, err = t.handleCapability(ctx, args)
	case "policy":
		out, err = t.handlePolicy(ctx, args)
	case "mission":
		out, err = t.handleMission(ctx, sess, args)
	case "clear":
		out, err = t.handleClear(ctx, sid, sess)
	case "compact":
		out, err = t.handleCompact(ctx, sid, sess, args)
	case "rename":
		out, err = t.handleRename(ctx, sess, args)
	default:
		err = libacp.NewErrorf(libacp.ErrInvalidParams, "unknown command %q", name)
	}

	if err != nil {
		reportErr(err)
		t.sendUpdate(ctx, libacp.SessionNotification{
			SessionID: sid,
			Update:    libacp.NewAgentMessageChunk("⚠️  " + err.Error()),
		})
		t.persistCommandTurn(ctx, sess, name, args, "⚠️  "+err.Error())
		return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
	}
	if out != "" {
		t.sendUpdate(ctx, libacp.SessionNotification{
			SessionID: sid,
			Update:    libacp.NewAgentMessageChunk(out),
		})
	}
	t.persistCommandTurn(ctx, sess, name, args, out)
	if commandUpdatesSessionInfo(name) {
		// An ordinary turn pushes session_info_update from Prompt's own
		// AfterResponse; a command returns before ever reaching it, so a
		// command that CHANGES the session's label has to push its own — else
		// the operator renames a session and watches the old label sit there
		// until a full re-list. Deferred past the response for the same reason
		// Prompt defers its: a client maps the update to a session it already
		// knows about, in the order the wire delivered.
		libacp.AfterResponse(ctx, func() {
			update := libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateSessionInfo,
				UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
			}
			if title := t.sessionInfoTitle(ctx, sess.InternalSessionID); title != "" {
				update.Title = title
			}
			t.sendUpdate(ctx, libacp.SessionNotification{SessionID: sid, Update: update})
		})
	}
	if commandUpdatesSessionModel(name) {
		sess.setModelSelection(t.provider(), t.model())
	}
	if commandUpdatesConfigOptions(name) {
		t.sendConfigOptionUpdate(ctx, sid, sess)
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

// persistCommandTurn records a slash-command exchange — the line the operator
// typed and the answer they got — in the session's durable transcript, the same
// store an ordinary turn writes to.
//
// Without this a command is a WIRE EVENT ONLY: it renders once and is gone on
// reload. That was invisible for /help, and badly wrong for /mission, whose whole
// point is a durable act — an operator who fired a mission as a session's FIRST
// message came back to a session with no messages at all, which has no title (the
// sidebar falls back to a short id), no last-activity timestamp (so beam's
// freshest-first roster sorts it below every real chat), and an empty transcript.
// The session looked deleted. It was merely never written to.
//
// The two history-rewriting commands are excluded, because for them the
// transcript is the OUTPUT, not the record: /clear empties it (appending the
// command that emptied it would leave the one line it just promised to remove)
// and /compact replaces it with a summary.
func (t *Transport) persistCommandTurn(ctx context.Context, sess *sessionEntry, name, args, out string) {
	if t.deps.DB == nil || sess == nil || commandRewritesHistory(name) {
		return
	}
	internalID := sess.InternalSessionID
	if internalID == "" || strings.TrimSpace(out) == "" {
		return
	}
	typed := "/" + name
	if args = strings.TrimSpace(args); args != "" {
		typed += " " + args
	}
	now := time.Now().UTC()
	msgs := []taskengine.Message{
		{ID: uuid.NewString(), Role: "user", Content: typed, Timestamp: now},
		{ID: uuid.NewString(), Role: "assistant", Content: out, Timestamp: now.Add(time.Millisecond)},
	}
	cleanCtx := context.WithoutCancel(ctx)
	mgr := chatservice.NewManager(sess.WorkspaceID)
	if err := mgr.PersistDiff(cleanCtx, t.deps.DB.WithoutTransaction(), internalID, msgs); err != nil {
		reportErr, _, end := t.tracker().Start(cleanCtx, "persist", "acp_command_turn", "session_id", internalID, "command", name)
		reportErr(err)
		end()
	}
}

// commandRewritesHistory reports whether a command owns the transcript itself, in
// which case persistCommandTurn must not append to it. See there.
func commandRewritesHistory(name string) bool {
	switch name {
	case "clear", "compact":
		return true
	default:
		return false
	}
}

// commandUpdatesSessionInfo reports whether a command changed what the session
// is CALLED, which every attached client needs pushed rather than polled.
func commandUpdatesSessionInfo(name string) bool {
	return name == "rename"
}

func commandUpdatesSessionModel(name string) bool {
	switch name {
	case "model", "provider":
		return true
	default:
		return false
	}
}

func commandUpdatesConfigOptions(name string) bool {
	switch name {
	case "model", "provider", "policy", "think":
		return true
	default:
		return false
	}
}

// sendAvailableCommands advertises the admin command set for a session. The
// client uses it to populate its slash-command menu. It MUST run only after the
// session's creation/load result has reached the client (callers schedule it via
// libacp.AfterResponse): a client maps the update to a session it already knows,
// and drops it otherwise — which silently disables the menu.
func (t *Transport) sendAvailableCommands(ctx context.Context, sid libacp.SessionID) {
	t.sendUpdate(ctx, libacp.SessionNotification{
		SessionID: sid,
		Update: libacp.SessionUpdate{
			SessionUpdate:     libacp.SessionUpdateAvailableCommands,
			AvailableCommands: t.acpCommands(),
		},
	})
}

func (t *Transport) handleHelp() string {
	cmds := t.acpCommands()
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	var b strings.Builder
	b.WriteString("Available commands:\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "  /%-9s %s\n", c.Name, c.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleDoctor reports current provider/model/backend readiness. It recomputes
// from live runtime state via the engine — read-only, never a model completion.
func (t *Transport) handleDoctor(ctx context.Context) (string, error) {
	if t.deps.Engine == nil || t.deps.Engine.SetupStatus == nil {
		return "", fmt.Errorf("readiness check unavailable")
	}
	res, err := t.deps.Engine.SetupStatus(ctx)
	if err != nil {
		return "", fmt.Errorf("readiness check failed: %w", err)
	}
	summary := res.Summary()

	// Advisory: warn when default-max-tokens exceeds the active provider's ceiling.
	ceiling := res.DefaultMaxOutputTokens
	if ceiling > 0 {
		maxTok := t.maxTokens()
		if maxTok != "" {
			if n, convErr := strconv.Atoi(maxTok); convErr == nil && n > ceiling {
				summary += fmt.Sprintf(
					"\n⚠️  Advisory: default-max-tokens=%d exceeds %s provider ceiling (%d). Requests will be clamped automatically.",
					n, t.provider(), ceiling)
			}
		}
	}
	return summary, nil
}

func (t *Transport) handleModel(ctx context.Context, args string) (string, error) {
	value := strings.TrimSpace(args)
	if value == "" {
		return fmt.Sprintf("Model: %s", t.model()), nil
	}
	if err := t.persistConfig(ctx, "default-model", value); err != nil {
		return "", err
	}
	t.setModel(value)
	return fmt.Sprintf("Model set to %s.", value), nil
}

func (t *Transport) handleProvider(ctx context.Context, args string) (string, error) {
	value := strings.TrimSpace(args)
	if value == "" {
		current := t.provider()
		if current == "" {
			return "Provider: (default)", nil
		}
		return fmt.Sprintf("Provider: %s", current), nil
	}
	if err := t.persistConfig(ctx, "default-provider", value); err != nil {
		return "", err
	}
	t.setProvider(value)
	return fmt.Sprintf("Provider set to %s.", value), nil
}

func (t *Transport) handleMaxTokens(ctx context.Context, args string) (string, error) {
	ceiling := t.maxOutputTokensCeiling(ctx)
	value := strings.TrimSpace(args)
	if value == "" {
		current := t.maxTokens()
		if current == "" {
			return fmt.Sprintf("Max tokens: (chain default) | provider ceiling: %s", ceilingLabel(ceiling)), nil
		}
		return fmt.Sprintf("Max tokens: %s | provider ceiling: %s", current, ceilingLabel(ceiling)), nil
	}
	normalized, err := normalizeMaxTokensValue(value)
	if err != nil {
		return "", err
	}
	if err := t.persistConfig(ctx, "default-max-tokens", normalized); err != nil {
		return "", err
	}
	t.setMaxTokens(normalized)
	if normalized == "" {
		return "Max tokens reset to chain default.", nil
	}
	msg := fmt.Sprintf("Max tokens set to %s.", normalized)
	if ceiling > 0 {
		n, _ := strconv.Atoi(normalized)
		if n > ceiling {
			msg += fmt.Sprintf(" ⚠️  Exceeds provider ceiling (%d) — requests will be clamped.", ceiling)
		}
	}
	return msg, nil
}

func (t *Transport) maxOutputTokensCeiling(ctx context.Context) int {
	if t.deps.Engine == nil || t.deps.Engine.State == nil {
		return 0
	}
	states := setupcheck.StatesFromMap(t.deps.Engine.State.Get(ctx))
	return setupcheck.ResolveMaxOutputTokens(states, t.provider(), t.model())
}

func ceilingLabel(ceiling int) string {
	if ceiling > 0 {
		return strconv.Itoa(ceiling)
	}
	return "unknown"
}

func normalizeMaxTokensValue(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return "", fmt.Errorf("max-tokens must be a non-negative integer, got %q", value)
	}
	if n < 0 {
		return "", fmt.Errorf("max-tokens must be non-negative, got %d", n)
	}
	return strconv.Itoa(n), nil
}

func (t *Transport) handleThink(sess *sessionEntry, args string) (string, error) {
	value := strings.TrimSpace(args)
	if value == "" {
		return fmt.Sprintf("Think: %s", sess.think()), nil
	}
	level, err := reasoning.Normalize(value)
	if err != nil {
		return "", err
	}
	sess.setThink(level)
	return fmt.Sprintf("Think set to %s for this session.", level), nil
}

func (t *Transport) handleCapability(ctx context.Context, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return t.capabilityUsage(ctx), nil
	}
	switch fields[0] {
	case "show":
		if len(fields) != 3 {
			return "", fmt.Errorf("usage: /capability show <provider> <model>")
		}
		return t.capabilityShow(ctx, fields[1], fields[2])
	case "set":
		provider, model, canThink, err := parseCapabilitySetArgs(fields)
		if err != nil {
			return "", err
		}
		store := runtimetypes.New(t.deps.DB.WithoutTransaction())
		override, err := modelcapability.New(store).SetThink(ctx, provider, model, canThink)
		if err != nil {
			return "", fmt.Errorf("set capability override: %w", err)
		}
		return fmt.Sprintf("Capability override set for %s/%s: think=%t.", override.Provider, override.Model, canThink), nil
	case "unset":
		if len(fields) != 3 {
			return "", fmt.Errorf("usage: /capability unset <provider> <model>")
		}
		store := runtimetypes.New(t.deps.DB.WithoutTransaction())
		removed, err := modelcapability.New(store).Unset(ctx, fields[1], fields[2])
		if err != nil {
			return "", fmt.Errorf("unset capability override: %w", err)
		}
		_, provider, model, keyErr := modelcapability.Key(fields[1], fields[2])
		if keyErr != nil {
			return "", keyErr
		}
		if !removed {
			return fmt.Sprintf("No capability override for %s/%s.", provider, model), nil
		}
		return fmt.Sprintf("Capability override removed for %s/%s.", provider, model), nil
	default:
		return "", fmt.Errorf("usage: /capability set|show|unset <provider> <model> [--think true|false]")
	}
}

func (t *Transport) capabilityUsage(ctx context.Context) string {
	usage := "Usage:\n  /capability show <provider> <model>\n  /capability set <provider> <model> --think true|false\n  /capability unset <provider> <model>\n\nThis persists a provider/model capability override. It is separate from /think, which only changes this session's reasoning level."
	provider := strings.TrimSpace(t.provider())
	model := strings.TrimSpace(t.model())
	if provider == "" || model == "" {
		return usage
	}
	status, err := t.capabilityShow(ctx, provider, model)
	if err != nil {
		return usage
	}
	return usage + "\n\nCurrent default:\n" + status
}

func (t *Transport) capabilityShow(ctx context.Context, provider, model string) (string, error) {
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	override, ok, err := modelcapability.New(store).Get(ctx, provider, model)
	if err != nil {
		return "", fmt.Errorf("show capability override: %w", err)
	}
	if !ok || override.CanThink == nil {
		_, p, m, keyErr := modelcapability.Key(provider, model)
		if keyErr != nil {
			return "", keyErr
		}
		return fmt.Sprintf("No capability override for %s/%s.", p, m), nil
	}
	return fmt.Sprintf("Capability override for %s/%s: think=%t.", override.Provider, override.Model, *override.CanThink), nil
}

func parseCapabilitySetArgs(fields []string) (string, string, bool, error) {
	if len(fields) < 4 {
		return "", "", false, fmt.Errorf("usage: /capability set <provider> <model> --think true|false")
	}
	provider, model := fields[1], fields[2]
	var canThink bool
	seenThink := false
	for i := 3; i < len(fields); i++ {
		arg := fields[i]
		value := ""
		if strings.HasPrefix(arg, "--think=") {
			value = strings.TrimPrefix(arg, "--think=")
		} else if arg == "--think" {
			if i+1 >= len(fields) {
				return "", "", false, fmt.Errorf("--think requires true or false")
			}
			i++
			value = fields[i]
		} else {
			return "", "", false, fmt.Errorf("unknown capability flag %q", arg)
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true":
			canThink = true
		case "false":
			canThink = false
		default:
			return "", "", false, fmt.Errorf("--think must be true or false")
		}
		seenThink = true
	}
	if !seenThink {
		return "", "", false, fmt.Errorf("--think is required")
	}
	return provider, model, canThink, nil
}

// handlePolicy shows or switches the active HITL approval policy. Switching
// writes the global cli.hitl-policy-name key — the key the engine reads live on
// every gated tool call — so the change takes effect on the next gated call. It
// just does what it's told: the operator owns the policy.
func (t *Transport) handlePolicy(ctx context.Context, args string) (string, error) {
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	value := strings.TrimSpace(args)
	if value == "" {
		return t.policyStatus(clikv.ReadHITLPolicy(ctx, store)), nil
	}
	cfgCtx := libtracker.WithNewRequestID(ctx)
	if err := clikv.SetHITLPolicy(cfgCtx, store, value); err != nil {
		return "", fmt.Errorf("set hitl policy: %w", err)
	}
	return fmt.Sprintf("HITL policy set to %s. Applies to the next gated tool call.", value), nil
}

// policyStatus renders the effective policy and the selectable presets. With no
// override set, the effective policy is the engine's fallback default.
func (t *Transport) policyStatus(active string) string {
	effective := active
	if effective == "" {
		effective = t.deps.HITLDefaultPolicyName
	}
	var b strings.Builder
	if active == "" {
		fmt.Fprintf(&b, "Active HITL policy: %s (default)\n", effective)
	} else {
		fmt.Fprintf(&b, "Active HITL policy: %s\n", effective)
	}
	if len(t.deps.KnownPolicies) > 0 {
		b.WriteString("Presets:\n")
		for _, name := range t.deps.KnownPolicies {
			marker := "  "
			if name == effective {
				marker = "* "
			}
			fmt.Fprintf(&b, "%s%s\n", marker, name)
		}
		b.WriteString("Switch with: /policy <name>")
	}
	return strings.TrimRight(b.String(), "\n")
}

// persistConfig writes a global CLI config value, mirroring `contenox config
// set` so the change also applies to future sessions and CLI invocations.
func (t *Transport) persistConfig(ctx context.Context, key, value string) error {
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	cfgCtx := libtracker.WithNewRequestID(ctx)
	if err := clikv.WriteConfig(cfgCtx, store, t.workspaceID(), key, value); err != nil {
		return fmt.Errorf("persist %s: %w", key, err)
	}
	return nil
}
