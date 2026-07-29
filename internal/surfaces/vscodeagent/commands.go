package vscodeagent

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/modelcapability"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

const compactDefaultKeep = 8

type slashCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Hint        string `json:"hint,omitempty"`
}

type listCommandsResult struct {
	Commands []slashCommand `json:"commands"`
}

func slashCommands() []slashCommand {
	return []slashCommand{
		{Name: "help", Description: "List the available commands."},
		{Name: "doctor", Description: "Check provider/model/backend readiness without sending a prompt."},
		{Name: "clear", Description: "Clear this session's conversation history."},
		{Name: "compact", Description: "Summarize older history into a single message to reclaim context.", Hint: "[keep]"},
		{Name: "model", Description: "Show or set the default model.", Hint: "[model-name]"},
		{Name: "provider", Description: "Show or set the default provider.", Hint: "[provider-name]"},
		{Name: "autocomplete-model", Description: "Show or set the default autocomplete model.", Hint: "[model-name]"},
		{Name: "autocomplete-provider", Description: "Show or set the default autocomplete provider.", Hint: "[provider-name]"},
		{Name: "max-tokens", Description: "Show or set the default response token cap.", Hint: "[count]"},
		{Name: "think", Description: "Show or set the reasoning level.", Hint: "[auto|off|minimal|low|medium|high|xhigh]"},
		{Name: "policy", Description: "Show or set the active HITL policy.", Hint: "[policy-name]"},
		{Name: "capability", Description: "Show/set provider/model capability overrides.", Hint: "set|show|unset <provider> <model> [--think true|false]"},
		{Name: "websearch", Description: "Search the web and return compact cited results.", Hint: "<query>"},
	}
}

var slashCommandNames = func() map[string]struct{} {
	out := make(map[string]struct{}, len(slashCommands()))
	for _, cmd := range slashCommands() {
		out[cmd.Name] = struct{}{}
	}
	return out
}()

func listSlashCommands() listCommandsResult {
	return listCommandsResult{Commands: slashCommands()}
}

func parseSlashCommand(input string) (name, args string, ok bool) {
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
	if _, known := slashCommandNames[first]; !known {
		return "", "", false
	}
	return first, args, true
}

func (s *Server) runCommandTurn(ctx context.Context, sessionID, turnID, name, args string) {
	reqID, _ := ctx.Value(libtracker.ContextKeyRequestID).(string)
	defer s.unregisterTurn(reqID, turnID)

	_ = s.notify("chatStarted", chatLifecycleEvent{SessionID: sessionID, TurnID: turnID})
	out, err := s.dispatchSlashCommand(ctx, sessionID, name, args)
	if err != nil {
		_ = s.notify("chatDelta", chatDeltaEvent{SessionID: sessionID, TurnID: turnID, Content: "Warning: " + err.Error()})
		_ = s.notify("chatCompleted", chatLifecycleEvent{SessionID: sessionID, TurnID: turnID, Messages: s.messagesOrEmpty(sessionID)})
		return
	}
	if out != "" {
		_ = s.notify("chatDelta", chatDeltaEvent{SessionID: sessionID, TurnID: turnID, Content: out})
	}
	_ = s.notify("chatCompleted", chatLifecycleEvent{SessionID: sessionID, TurnID: turnID, Messages: s.messagesOrEmpty(sessionID)})
}

func (s *Server) dispatchSlashCommand(ctx context.Context, sessionID, name, args string) (string, error) {
	switch name {
	case "help":
		return commandHelp(), nil
	case "doctor":
		return s.commandDoctor(ctx)
	case "clear":
		return s.commandClear(ctx, sessionID)
	case "compact":
		return s.commandCompact(ctx, sessionID, args)
	case "model":
		return s.commandConfig(ctx, "default-model", "Model", args, nil)
	case "provider":
		return s.commandConfig(ctx, "default-provider", "Provider", args, nil)
	case "autocomplete-model":
		return s.commandConfig(ctx, "default-autocomplete-model", "Autocomplete model", args, nil)
	case "autocomplete-provider":
		return s.commandConfig(ctx, "default-autocomplete-provider", "Autocomplete provider", args, nil)
	case "max-tokens":
		return s.commandConfig(ctx, "default-max-tokens", "Max tokens", args, normalizeMaxTokens)
	case "think":
		return s.commandThink(ctx, sessionID, args)
	case "policy":
		return s.commandPolicy(ctx, args)
	case "capability":
		return s.commandCapability(ctx, args)
	case "websearch":
		return s.commandWebSearch(ctx, args)
	default:
		return "", fmt.Errorf("unknown command %q", name)
	}
}

func commandHelp() string {
	cmds := slashCommands()
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	var b strings.Builder
	b.WriteString("Available commands:\n")
	for _, cmd := range cmds {
		if cmd.Hint != "" {
			fmt.Fprintf(&b, "  /%-10s %-32s %s\n", cmd.Name, cmd.Hint, cmd.Description)
		} else {
			fmt.Fprintf(&b, "  /%-10s %s\n", cmd.Name, cmd.Description)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (s *Server) commandDoctor(ctx context.Context) (string, error) {
	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return "", err
	}
	if rt.Engine == nil || rt.Engine.SetupStatus == nil {
		return "", fmt.Errorf("readiness check unavailable")
	}
	status, err := rt.Engine.SetupStatus(ctx)
	if err != nil {
		return "", fmt.Errorf("readiness check failed: %w", err)
	}
	return status.Summary(), nil
}

func (s *Server) commandClear(ctx context.Context, sessionID string) (string, error) {
	mgr := chatservice.NewManager(s.workspaceID)
	exec, commit, release, err := s.db.WithTransaction(ctx)
	if err != nil {
		return "", fmt.Errorf("start transaction: %w", err)
	}
	defer release()
	if err := mgr.ClearSession(ctx, exec, sessionID); err != nil {
		return "", err
	}
	if err := commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return "Conversation history cleared.", nil
}

func (s *Server) commandCompact(ctx context.Context, sessionID, args string) (string, error) {
	keep := compactDefaultKeep
	if value := strings.TrimSpace(args); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return "", fmt.Errorf("invalid keep count %q: expected a non-negative integer", value)
		}
		keep = n
	}

	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return "", err
	}
	if rt.Engine == nil || rt.Engine.TaskService == nil || rt.CompactChain == nil {
		return "", fmt.Errorf("compaction chain unavailable")
	}

	mgr := chatservice.NewManager(s.workspaceID)
	history, err := mgr.ListMessages(ctx, s.db.WithoutTransaction(), sessionID)
	if err != nil {
		return "", fmt.Errorf("load history: %w", err)
	}
	if len(history) == 0 {
		return "", fmt.Errorf("no history to compact")
	}

	vars := s.templateVars(ctx, sessionID)
	vars["chain"] = rt.CompactChain.ID
	execCtx := taskengine.WithTemplateVars(ctx, vars)
	compacted, err := chatservice.CompactHistory(execCtx, rt.Engine.TaskService, rt.CompactChain, history, keep)
	if err != nil {
		return "", err
	}

	exec, commit, release, err := s.db.WithTransaction(ctx)
	if err != nil {
		return "", fmt.Errorf("start transaction: %w", err)
	}
	defer release()
	if err := mgr.ClearSession(ctx, exec, sessionID); err != nil {
		return "", err
	}
	if err := mgr.PersistDiff(ctx, exec, sessionID, compacted); err != nil {
		return "", fmt.Errorf("persist compacted history: %w", err)
	}
	if err := commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return fmt.Sprintf("Compacted %d messages to %d (kept last %d).", len(history), len(compacted), keep), nil
}

func (s *Server) commandConfig(ctx context.Context, key, label, args string, normalize func(string) (string, error)) (string, error) {
	value := strings.TrimSpace(args)
	if value == "" {
		current, _ := clikv.ReadConfig(ctx, s.store, s.workspaceID, key)
		if current == "" {
			return label + ": (default)", nil
		}
		return label + ": " + current, nil
	}
	if normalize != nil {
		normalized, err := normalize(value)
		if err != nil {
			return "", err
		}
		value = normalized
	}
	if err := clikv.WriteConfig(ctx, s.store, s.workspaceID, key, value); err != nil {
		return "", fmt.Errorf("persist %s: %w", key, err)
	}
	s.resetRuntime()
	_ = s.notify("configChanged", s.getConfig(ctx))
	return fmt.Sprintf("%s set to %s.", label, value), nil
}

func normalizeMaxTokens(value string) (string, error) {
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

func (s *Server) commandThink(ctx context.Context, sessionID, args string) (string, error) {
	value := strings.TrimSpace(args)
	if value == "" {
		return fmt.Sprintf("Think: %s", s.effectiveThink(ctx, sessionID)), nil
	}
	level, err := reasoning.Normalize(value)
	if err != nil {
		return "", err
	}
	s.setSessionThink(sessionID, level)
	return fmt.Sprintf("Think set to %s for this session.", level), nil
}

func (s *Server) commandPolicy(ctx context.Context, args string) (string, error) {
	value := strings.TrimSpace(args)
	if value == "" {
		active := clikv.ReadHITLPolicy(ctx, s.store)
		if active == "" {
			active = defaultHITLPolicyName
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Active HITL policy: %s\n", active)
		if len(s.policyNames) > 0 {
			b.WriteString("Presets:\n")
			for _, name := range s.policyNames {
				marker := "  "
				if name == active {
					marker = "* "
				}
				fmt.Fprintf(&b, "%s%s\n", marker, name)
			}
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	if err := clikv.SetHITLPolicy(ctx, s.store, value); err != nil {
		return "", fmt.Errorf("set hitl policy: %w", err)
	}
	s.resetRuntime()
	_ = s.notify("configChanged", s.getConfig(ctx))
	return fmt.Sprintf("HITL policy set to %s. Applies to the next gated tool call.", value), nil
}

func (s *Server) commandCapability(ctx context.Context, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "Usage:\n  /capability show <provider> <model>\n  /capability set <provider> <model> --think true|false\n  /capability unset <provider> <model>", nil
	}
	store := runtimetypes.New(s.db.WithoutTransaction())
	switch fields[0] {
	case "show":
		if len(fields) != 3 {
			return "", fmt.Errorf("usage: /capability show <provider> <model>")
		}
		override, ok, err := modelcapability.New(store).Get(ctx, fields[1], fields[2])
		if err != nil {
			return "", err
		}
		if !ok || override.CanThink == nil {
			return fmt.Sprintf("No capability override for %s/%s.", fields[1], fields[2]), nil
		}
		return fmt.Sprintf("Capability override for %s/%s: think=%t.", override.Provider, override.Model, *override.CanThink), nil
	case "set":
		provider, model, canThink, err := parseCapabilitySetArgs(fields)
		if err != nil {
			return "", err
		}
		override, err := modelcapability.New(store).SetThink(ctx, provider, model, canThink)
		if err != nil {
			return "", err
		}
		s.resetRuntime()
		return fmt.Sprintf("Capability override set for %s/%s: think=%t.", override.Provider, override.Model, canThink), nil
	case "unset":
		if len(fields) != 3 {
			return "", fmt.Errorf("usage: /capability unset <provider> <model>")
		}
		removed, err := modelcapability.New(store).Unset(ctx, fields[1], fields[2])
		if err != nil {
			return "", err
		}
		s.resetRuntime()
		if !removed {
			return fmt.Sprintf("No capability override for %s/%s.", fields[1], fields[2]), nil
		}
		return fmt.Sprintf("Capability override removed for %s/%s.", fields[1], fields[2]), nil
	default:
		return "", fmt.Errorf("usage: /capability set|show|unset <provider> <model> [--think true|false]")
	}
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

func (s *Server) messagesOrEmpty(sessionID string) []messageInfo {
	messages, err := s.messagesForSession(context.Background(), sessionID)
	if err != nil {
		return nil
	}
	return messages
}
