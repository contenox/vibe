// Package missiontools is the per-mission tool grant a dispatched unit holds while running unattended: report progress, ask for attention, maintain a living plan, end with a verdict, and heartbeat. The mission id rides the request context (WithMissionID) rather than being a model argument, so the grant is unforgeable and scoped to the caller's own mission. The package's responsibility ends at the missionservice write succeeding; it does not route reports or project the plan onto any transport stream.
package missiontools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missionservice"
)

// ToolsProviderName is the tools-provider key this package registers under, exposing ToolNameReport, ToolNameAskAttention, ToolNamePlan, and ToolNameFinish.
const ToolsProviderName = "mission"

const (
	// ToolNameReport files a structured progress/finding/blocker/result report
	// against the caller's own mission.
	ToolNameReport = "mission_report"
	// ToolNameAskAttention flags that the caller's mission needs a decision it
	// may not make on its own — answered by a human, by the supervising
	// session's agent, or by the oracle, whichever reaches it first.
	ToolNameAskAttention = "mission_ask_attention"
	// ToolNamePlan replaces the caller's mission plan with a full snapshot,
	// routing to missionservice.SetPlan.
	ToolNamePlan = "mission_plan"
	// ToolNameFinish brings the caller's mission to rest in a terminal state,
	// routing to missionservice.Finish.
	ToolNameFinish = "mission_finish"
)

type missionCtxKey struct{}

// WithMissionID binds missionID to ctx as the caller's mission, called once by the transport when it builds a dispatched unit's session; an empty id returns ctx unchanged.
func WithMissionID(ctx context.Context, missionID string) context.Context {
	if strings.TrimSpace(missionID) == "" {
		return ctx
	}
	return context.WithValue(ctx, missionCtxKey{}, missionID)
}

// MissionIDFromContext returns the mission id bound by WithMissionID, or ""
// when the caller is not on a mission.
func MissionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(missionCtxKey{}).(string)
	return id
}

// MissionStore is the narrow slice of missionservice.Service these tools need (report, plan, finish, liveness), deliberately not the full Service since a unit's back-channel must not create, list, bind, or delete missions.
type MissionStore interface {
	AddReport(ctx context.Context, missionID string, report *missionservice.Report) error
	Heartbeat(ctx context.Context, id string, lastErr string) (*missionservice.Mission, error)
	// SetPlan returns the stored mission, whose Plan carries the ids SetPlan
	// assigned to id-less entries — echoed to the caller so the next
	// revision can carry ids forward.
	SetPlan(ctx context.Context, id string, entries []missionservice.PlanEntry, explanation string) (*missionservice.Mission, error)
	// Finish moves the mission into a terminal state (guarded, immutable).
	Finish(ctx context.Context, id string, status missionservice.Status, reason string) (*missionservice.Mission, error)
}

// AttentionAsker is the durable-ask channel mission_ask_attention runs on; unset, the tool files a blocker report instead.
type AttentionAsker interface {
	RaiseAttention(ctx context.Context, ask AttentionAsk) (string, error)
}

type AttentionAsk struct {
	MissionID string
	Summary   string
	Detail    string
	// AskID is the durable row identity, the engine tool-call ID on the suspendable path.
	AskID string
}

type provider struct {
	missions        MissionStore
	asker           AttentionAsker
	supervisor      SupervisorStore
	resolver        AttentionResolver
	recordDowngrade func()

	// The supervisor half: what a session that HAS subagents may start and watch.
	spawner          Spawner
	watcher          SubagentWatcher
	subagentDefaults SubagentDefaults
	subagentTimeout  time.Duration
}

// Option configures the provider at construction.
type Option func(*provider)

// WithAttentionAsker wires the durable-ask channel mission_ask_attention runs on.
func WithAttentionAsker(asker AttentionAsker) Option {
	return func(p *provider) {
		p.asker = asker
	}
}

// WithSupervision wires the tool a session that fired missions gets: list your missions.
func WithSupervision(store SupervisorStore) Option {
	return func(p *provider) {
		p.supervisor = store
	}
}

// WithAttentionResolver adds the answering half of the supervisor surface: read what your subagents are waiting on, and answer it.
func WithAttentionResolver(resolver AttentionResolver) Option {
	return func(p *provider) {
		p.resolver = resolver
	}
}

// New returns the mission-tools provider; missions is required and New panics on nil rather than degrade.
func New(missions MissionStore, opts ...Option) taskengine.ToolsRepo {
	if missions == nil {
		panic("missiontools: mission store is required")
	}
	p := &provider{missions: missions, recordDowngrade: func() {}}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(p)
	}
	return p
}

func (p *provider) Supports(context.Context) ([]string, error) {
	return []string{ToolsProviderName}, nil
}

func (p *provider) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != ToolsProviderName {
		return nil, fmt.Errorf("unknown tools: %s", name)
	}
	if MissionIDFromContext(ctx) != "" {
		return []taskengine.Tool{
			reportToolSchema(),
			askAttentionToolSchema(),
			planToolSchema(),
			finishToolSchema(),
		}, nil
	}
	// Not a mission: maybe a session that supervises some, so offer the smaller supervisor surface.
	if p.supervisor != nil && ParentSessionIDFromContext(ctx) != "" {
		return p.supervisorTools(), nil
	}
	return []taskengine.Tool{}, nil
}

func (p *provider) Exec(ctx context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if call == nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: missing tools call")
	}
	// Supervisor tools are checked first so they aren't rejected by the unit-only gate below.
	switch call.ToolName {
	case ToolNameListMissions, ToolNameAnswer:
		parentSessionID := ParentSessionIDFromContext(ctx)
		if parentSessionID == "" {
			return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %s is only available to a session that supervises missions", call.ToolName)
		}
		if call.ToolName == ToolNameListMissions {
			return p.execListMissions(ctx, parentSessionID)
		}
		return p.execAnswer(ctx, parentSessionID, input, call)
	case ToolNameStartMission:
		parentSessionID := ParentSessionIDFromContext(ctx)
		if parentSessionID == "" {
			return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %s is only available to a session that supervises missions; a subagent may not start subagents of its own", call.ToolName)
		}
		return p.execStartMission(ctx, parentSessionID, input, call)
	}
	missionID := MissionIDFromContext(ctx)
	if missionID == "" {
		return nil, taskengine.DataTypeAny, fmt.Errorf("mission tools are only available to a unit dispatched on a mission")
	}
	switch call.ToolName {
	case ToolNameReport:
		return p.execReport(ctx, missionID, input, call)
	case ToolNameAskAttention:
		return p.execAskAttention(ctx, missionID, input, call)
	case ToolNamePlan:
		return p.execPlan(ctx, missionID, input, call)
	case ToolNameFinish:
		return p.execFinish(ctx, missionID, input, call)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: unknown tool %q (want %s, %s, %s or %s)", call.ToolName, ToolNameReport, ToolNameAskAttention, ToolNamePlan, ToolNameFinish)
	}
}

func (p *provider) execReport(ctx context.Context, missionID string, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	kind := missionservice.ReportKind(argString(input, call, "kind"))
	if strings.TrimSpace(string(kind)) == "" {
		// No kind named defaults to progress; a malformed kind still fails loudly in AddReport's validation.
		kind = missionservice.ReportKindProgress
	}
	handover, err := parseHandover(input, call)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	report := &missionservice.Report{
		Kind:     kind,
		Summary:  argString(input, call, "summary"),
		Detail:   argString(input, call, "detail"),
		Refs:     argStrings(input, call, "refs"),
		Handover: handover,
	}
	// A result whose claimed artifacts include a missing path is downgraded to progress before the write.
	downgradeNote := ""
	if report.Kind == missionservice.ReportKindResult {
		claims := reportClaims{refs: report.Refs}
		if report.Handover != nil {
			claims.artifacts = report.Handover.Artifacts
		}
		if missing := missingArtifacts(WorkdirFromContext(ctx), claimedRefs(claims)); len(missing) > 0 {
			report.Kind = missionservice.ReportKindProgress
			report.Detail = appendWarning(report.Detail, verificationWarning(missing))
			downgradeNote = fmt.Sprintf(" (downgraded from result: %s: %s)", verificationWarningLead, quoteList(missing))
			p.recordDowngrade()
		}
	}
	if err := p.missions.AddReport(ctx, missionID, report); err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: file report: %w", err)
	}
	p.heartbeat(ctx, missionID)
	return fmt.Sprintf("recorded %s report %q%s", report.Kind, report.ID, downgradeNote), taskengine.DataTypeString, nil
}

func appendWarning(detail, warning string) string {
	if strings.TrimSpace(detail) == "" {
		return warning
	}
	return detail + "\n\n" + warning
}

func (p *provider) execAskAttention(ctx context.Context, missionID string, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	summary := argString(input, call, "summary")
	detail := argString(input, call, "detail")
	if strings.TrimSpace(summary) == "" {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: mission_ask_attention requires a summary")
	}
	callID, _ := ctx.Value(taskengine.ContextKeyToolCallID).(string)

	if ans, ok := taskengine.AttentionAnswerFromContext(ctx, callID); ok {
		p.heartbeat(ctx, missionID)
		if ans.Answered && strings.TrimSpace(ans.Text) != "" {
			return ans.Text, taskengine.DataTypeString, nil
		}
		detail = withUnansweredNote(detail, fmt.Errorf("the ask was resolved without an answer while the run was suspended"))
	} else if p.asker != nil {
		p.heartbeat(ctx, missionID)
		ask := AttentionAsk{MissionID: missionID, Summary: summary, Detail: detail}
		if callID != "" && taskengine.ToolCallSuspendable(ctx) && taskengine.HasCheckpointSaver(ctx) {
			ask.AskID = callID
		}
		answer, err := p.asker.RaiseAttention(ctx, ask)
		if err == nil {
			p.heartbeat(ctx, missionID)
			return answer, taskengine.DataTypeString, nil
		}
		var pending *taskengine.ApprovalPendingError
		if errors.As(err, &pending) {
			return nil, taskengine.DataTypeAny, pending
		}
		detail = withUnansweredNote(detail, err)
	}
	report := &missionservice.Report{Kind: missionservice.ReportKindBlocker, Summary: summary, Detail: detail}
	if err := p.missions.AddReport(ctx, missionID, report); err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: record attention request: %w", err)
	}
	p.heartbeat(ctx, missionID)
	return "attention requested (recorded as blocker — nobody answered)", taskengine.DataTypeString, nil
}

func withUnansweredNote(detail string, err error) string {
	note := fmt.Sprintf("(the unit asked for a decision and got no answer: %v)", err)
	if strings.TrimSpace(detail) == "" {
		return note
	}
	return detail + "\n\n" + note
}

func (p *provider) execPlan(ctx context.Context, missionID string, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	entries, err := parsePlanEntries(input, call)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	explanation := argString(input, call, "explanation")
	m, err := p.missions.SetPlan(ctx, missionID, entries, explanation)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: set plan: %w", err)
	}
	p.heartbeat(ctx, missionID)
	// The returned Plan is also what the transport reads to emit the ACP snapshot update.
	return m.Plan, taskengine.DataTypeJSON, nil
}

func (p *provider) execFinish(ctx context.Context, missionID string, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	status := missionservice.Status(strings.TrimSpace(argString(input, call, "status")))
	if status == "" {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: mission_finish requires a status (landed|derailed|stuck|abandoned)")
	}
	reason := argString(input, call, "reason")
	m, err := p.missions.Finish(ctx, missionID, status, reason)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: finish mission: %w", err)
	}
	p.heartbeat(ctx, missionID)
	return fmt.Sprintf("mission finished as %s", m.Status), taskengine.DataTypeString, nil
}

func (p *provider) heartbeat(ctx context.Context, missionID string) {
	_, _ = p.missions.Heartbeat(ctx, missionID, "")
}

func reportToolSchema() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        ToolNameReport,
			Description: "File a structured report on your CURRENT mission. Use it to record meaningful progress, a finding, a blocker, or a final result — not routine narration. You may only report on your own mission.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind": map[string]any{
						"type":        "string",
						"enum":        []string{string(missionservice.ReportKindProgress), string(missionservice.ReportKindFinding), string(missionservice.ReportKindBlocker), string(missionservice.ReportKindResult)},
						"description": "The shape of the report: progress, finding, blocker, or result. Omitted or blank files the report as progress.",
					},
					"summary": map[string]any{
						"type":        "string",
						"description": "A single-line summary of what is being reported.",
					},
					"detail": map[string]any{
						"type":        "string",
						"description": "Optional longer detail.",
					},
					"refs": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional references (file paths or URLs) — pointers only, never inline content.",
					},
					"handover": map[string]any{
						"type":        "object",
						"description": "Optional structured hand-off. Fill it on a `result` that a FOLLOW-UP mission will build on, so the next unit starts from real context instead of re-deriving yours. Skip it for routine progress or a self-contained result — an unfilled hand-off is the norm, not an omission.",
						"properties": map[string]any{
							"outcome": map[string]any{
								"type":        "string",
								"description": "One line: what this mission actually achieved — the hand-off's headline.",
							},
							"artifacts": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "The concrete deliverables the next mission consumes — file paths or URLs, by reference only, never inline content.",
							},
							"handoverForNext": map[string]any{
								"type":        "string",
								"description": "The brief to the next mission: what to pick up, what is already done, what to watch for.",
							},
							"caveats": map[string]any{
								"type":        "string",
								"description": "Known limitations, unverified assumptions, or risks the next mission must not take for granted.",
							},
						},
					},
				},
				"required": []string{"summary"},
			},
		},
	}
}

func askAttentionToolSchema() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        ToolNameAskAttention,
			Description: "ASK for a decision you may not make on your own, and WAIT for the reply — use it when you cannot proceed: a judgement outside your intent, a missing fact, an ambiguous instruction. The call BLOCKS until someone answers, and their answer comes back to you as this tool's result, so you continue with it on the same turn. If nobody answers in time your question is recorded as a blocker instead. Use it sparingly — it costs attention and time — but prefer it over guessing or giving up.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{
						"type":        "string",
						"description": "A single-line summary of the decision you need.",
					},
					"detail": map[string]any{
						"type":        "string",
						"description": "Optional longer detail: what you already tried, and what the options are.",
					},
				},
				"required": []string{"summary"},
			},
		},
	}
}

func planToolSchema() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        ToolNamePlan,
			Description: "Replace your CURRENT mission's plan with a FULL SNAPSHOT of it. Every call sends the ENTIRE plan, not a delta: an entry you leave out is deleted, an entry you include is kept or updated. Carry an entry forward by echoing the `id` it was given back to you; introduce a new one by omitting `id` (the runtime assigns it and returns it). The result echoes the stored plan with all ids — use those ids on your next revision. Give a short `explanation` whenever the plan changes shape. Completed entries are immutable: to correct finished work, add a new entry rather than editing the old one's text.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entries": map[string]any{
						"type":        "array",
						"description": "The complete, ordered list of plan entries — the whole plan, every time.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]any{
									"type":        "string",
									"description": "The entry's stable id. Echo the id you were given to carry an entry forward; omit it for a new entry.",
								},
								"content": map[string]any{
									"type":        "string",
									"description": "The step, in a few words. No filler, no single-step plans.",
								},
								"status": map[string]any{
									"type":        "string",
									"enum":        []string{string(missionservice.PlanEntryPending), string(missionservice.PlanEntryInProgress), string(missionservice.PlanEntryCompleted)},
									"description": "pending, in_progress, or completed.",
								},
								"priority": map[string]any{
									"type":        "string",
									"enum":        []string{string(missionservice.PlanEntryPriorityHigh), string(missionservice.PlanEntryPriorityMedium), string(missionservice.PlanEntryPriorityLow)},
									"description": "high, medium, or low.",
								},
							},
							"required": []string{"content", "status", "priority"},
						},
					},
					"explanation": map[string]any{
						"type":        "string",
						"description": "A one-line rationale for this revision — what changed and why.",
					},
				},
				"required": []string{"entries"},
			},
		},
	}
}

func finishToolSchema() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        ToolNameFinish,
			Description: "End your CURRENT mission with a verdict. This is TERMINAL and IMMUTABLE — once finished, a mission does not move again, so call it exactly once, when the work is truly over. Use `landed` when the mission succeeded; `derailed` when it failed and needs a post-mortem; `stuck` when you have hit a wall, a loop, or a judgement you may not make unattended — a boundary that asks for a human's attention rather than a failure report. Prefer mission_ask_attention while there is still work to resume; reserve `stuck` for when you genuinely cannot proceed. (`abandoned` is normally the operator's label, not yours.) Give a one-line `reason` for anything but a clean landing.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{
						"type":        "string",
						"enum":        []string{string(missionservice.StatusLanded), string(missionservice.StatusDerailed), string(missionservice.StatusStuck), string(missionservice.StatusAbandoned)},
						"description": "The terminal verdict: landed, derailed, stuck, or abandoned.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "A single-line reason for the outcome (why it derailed or got stuck).",
					},
				},
				"required": []string{"status"},
			},
		},
	}
}

type handoverArg struct {
	Outcome         string   `json:"outcome"`
	Artifacts       []string `json:"artifacts"`
	HandoverForNext string   `json:"handoverForNext"`
	Caveats         string   `json:"caveats"`
}

func parseHandover(input any, call *taskengine.ToolsCall) (*missionservice.Handover, error) {
	var arg handoverArg
	if m, ok := input.(map[string]any); ok {
		if v, ok := m["handover"]; ok {
			if err := decodeHandover(v, &arg); err != nil {
				return nil, err
			}
			return toHandover(arg), nil
		}
	}
	if call != nil && call.Args != nil {
		if v, ok := call.Args["handover"]; ok {
			if strings.TrimSpace(v) == "" {
				return nil, nil
			}
			if err := json.Unmarshal([]byte(v), &arg); err != nil {
				return nil, fmt.Errorf("missiontools: mission_report 'handover' must be a JSON object: %w", err)
			}
			return toHandover(arg), nil
		}
	}
	return nil, nil
}

func decodeHandover(v any, out *handoverArg) error {
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		if err := json.Unmarshal([]byte(s), out); err != nil {
			return fmt.Errorf("missiontools: mission_report 'handover' must be a JSON object: %w", err)
		}
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("missiontools: mission_report 'handover' could not be read: %w", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("missiontools: mission_report 'handover' must be a {outcome, artifacts, handoverForNext, caveats} object: %w", err)
	}
	return nil
}

func toHandover(a handoverArg) *missionservice.Handover {
	return &missionservice.Handover{
		Outcome:         strings.TrimSpace(a.Outcome),
		Artifacts:       trimStrings(a.Artifacts),
		HandoverForNext: strings.TrimSpace(a.HandoverForNext),
		Caveats:         strings.TrimSpace(a.Caveats),
	}
}

func trimStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type planEntryArg struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

func parsePlanEntries(input any, call *taskengine.ToolsCall) ([]missionservice.PlanEntry, error) {
	var args []planEntryArg
	if m, ok := input.(map[string]any); ok {
		if v, ok := m["entries"]; ok {
			if err := decodePlanEntries(v, &args); err != nil {
				return nil, err
			}
			return toPlanEntries(args), nil
		}
	}
	if call != nil && call.Args != nil {
		if v, ok := call.Args["entries"]; ok {
			if err := json.Unmarshal([]byte(v), &args); err != nil {
				return nil, fmt.Errorf("missiontools: mission_plan 'entries' must be a JSON array: %w", err)
			}
			return toPlanEntries(args), nil
		}
	}
	return nil, fmt.Errorf("missiontools: mission_plan requires an 'entries' array (a full snapshot of the plan)")
}

func decodePlanEntries(v any, out *[]planEntryArg) error {
	if s, ok := v.(string); ok {
		if err := json.Unmarshal([]byte(s), out); err != nil {
			return fmt.Errorf("missiontools: mission_plan 'entries' must be a JSON array: %w", err)
		}
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("missiontools: mission_plan 'entries' could not be read: %w", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("missiontools: mission_plan 'entries' must be a list of {content, status, priority} objects: %w", err)
	}
	return nil
}

func toPlanEntries(args []planEntryArg) []missionservice.PlanEntry {
	entries := make([]missionservice.PlanEntry, len(args))
	for i, a := range args {
		entries[i] = missionservice.PlanEntry{
			ID:       strings.TrimSpace(a.ID),
			Content:  a.Content,
			Status:   missionservice.PlanEntryStatus(strings.TrimSpace(a.Status)),
			Priority: missionservice.PlanEntryPriority(strings.TrimSpace(a.Priority)),
		}
	}
	return entries
}

func argString(input any, call *taskengine.ToolsCall, key string) string {
	if m, ok := input.(map[string]any); ok {
		if v, ok := m[key]; ok {
			return toStringValue(v)
		}
	}
	if call != nil && call.Args != nil {
		if v, ok := call.Args[key]; ok {
			return v
		}
	}
	return ""
}

func argStrings(input any, call *taskengine.ToolsCall, key string) []string {
	if m, ok := input.(map[string]any); ok {
		if v, ok := m[key]; ok {
			switch t := v.(type) {
			case []any:
				out := make([]string, 0, len(t))
				for _, e := range t {
					if s := strings.TrimSpace(toStringValue(e)); s != "" {
						out = append(out, s)
					}
				}
				return out
			case string:
				return splitRefs(t)
			}
		}
	}
	if call != nil && call.Args != nil {
		if v, ok := call.Args[key]; ok {
			return splitRefs(v)
		}
	}
	return nil
}

func splitRefs(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func toStringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

var _ taskengine.ToolsRepo = (*provider)(nil)
