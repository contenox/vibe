// Package missiontools is the per-mission tool grant a dispatched unit holds
// while it runs unattended: the channels the fleet blueprint's "mission mode"
// gives a unit for reaching back to an absent operator — report progress, ask
// for attention, maintain a living plan, and end the mission with a verdict —
// plus the liveness heartbeat that lets the operator tell a working unit from a
// dead one without watching a transcript
// (docs/development/blueprints/acp/fleet-consolidation.md, "Mission mode",
// slice M3; docs/development/blueprints/acp/mission-plans.md, slice 2).
//
// mission_plan and mission_finish are the plan engine's two back-channels: a
// resident planner rewrites its whole plan each call (mission_plan → SetPlan,
// full-snapshot replace) and, when the work comes to rest, records the terminal
// verdict (mission_finish → Finish). Both are held by the mission exactly as the
// report tools are — scoped to the caller's own mission id, invisible off a
// mission — so the plan engine reuses the report tools' grant shape rather than
// inventing a second one.
//
// # Why this is a tool, and why per-mission
//
// In chat mode you ARE the envelope: you see every step and answer every
// permission request live. In mission mode a unit runs with no viewer attached
// by design, so it needs a way to reach you that is not a live session. These
// tools ARE that way. They are granted BY THE MISSION, not by the agent: an
// agent has no standing `mission_report` in its tool set — it acquires one only
// for the duration of a mission, scoped to THAT mission's id. That keeps the
// grant per-unit-of-work (the same equip-don't-govern shape harnesses use) and,
// critically, keeps a unit from ever reporting against a mission that is not its
// own.
//
// # The envelope is enforced at construction, not by a check
//
// The mission id is not an argument the agent passes ("report on mission X") and
// then we verify — that would be a check-ordering bug waiting to happen. It
// rides the request context (WithMissionID), placed there by the transport that
// built the unit's session from the mission id it received at session/new (see
// missionservice.MissionMetaKey). A session that was not constructed on a
// mission carries no mission id, so:
//
//   - GetToolsForToolsByName returns NOTHING — the tools are absent from the
//     model's tool list, not present-but-refused. A unit not on a mission has
//     nothing to call.
//   - Exec, reached directly by a deterministic `tools` task that bypasses the
//     tool-list step, refuses with a clear error as a backstop.
//
// So the grant is unforgeable from the agent's side: the mission id it reports
// against is the one bound into its session, never one it names.
//
// # Boundary
//
// This package's responsibility ENDS at the missionservice write succeeding
// (AddReport / SetPlan / Finish) and at handing an attention ask to the
// durable-ask machinery. It does not route reports, deliver them to an operator
// inbox, project the plan onto any ACP stream, or subscribe to anything — those
// are downstream slices. The plan's ACP full-snapshot projection lives in the
// transport (acpsvc translates the mission_plan tool event into a `plan` session
// update): missionservice stays a store, this package stays a tool grant, and
// neither reaches into a transport. It reuses missionservice for every durable
// write and an injected AttentionAsker for attention, inventing no parallel
// store.
package missiontools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/getkin/kin-openapi/openapi3"
)

// ToolsProviderName is the tools-provider key this package registers under (the
// `name` a chain's `tools` task or a runtime allowlist refers to). The tools it
// exposes are ToolNameReport, ToolNameAskAttention, ToolNamePlan and
// ToolNameFinish.
const ToolsProviderName = "mission"

const (
	// ToolNameReport files a structured progress/finding/blocker/result report
	// against the caller's own mission.
	ToolNameReport = "mission_report"
	// ToolNameAskAttention flags that the caller's mission needs an operator.
	ToolNameAskAttention = "mission_ask_attention"

	// AttentionParkWindow is how long a suspendable mission_ask_attention
	// blocks before checkpoint-and-release — the question's twin of
	// localtools.ApprovalParkWindow, and the same duration on purpose: the
	// operator-is-watching fast path answers within it, everyone else answers
	// a durable row later.
	AttentionParkWindow = 30 * time.Second
	// ToolNamePlan replaces the caller's mission plan with a full snapshot (the
	// resident-planner channel: the plan is a living record it rewrites in whole,
	// never a schedule the runtime executes — see the plan blueprint's design
	// law). It routes to missionservice.SetPlan.
	ToolNamePlan = "mission_plan"
	// ToolNameFinish brings the caller's mission to rest in a terminal state with
	// a verdict. It routes to missionservice.Finish — the guarded, immutable,
	// agent-reportable transition, distinct from an operator's manual relabel.
	ToolNameFinish = "mission_finish"
)

// missionCtxKey is an unexported context key for the caller's mission id, so it
// cannot collide with any other package's context values. Mirrors
// taskengine.toolsArgsKey / hitlservice.WithPolicyName: the transport sets it
// once when it builds the unit's session, and it threads synchronously down to
// the tool's Exec.
type missionCtxKey struct{}

// WithMissionID binds missionID to ctx as the caller's mission. The transport
// that constructs a dispatched unit's session calls this once from the mission
// id it received at session/new; every tool call on that turn inherits it. An
// empty id returns ctx unchanged, so a non-mission session is never marked as
// being on a (blank) mission.
func WithMissionID(ctx context.Context, missionID string) context.Context {
	if strings.TrimSpace(missionID) == "" {
		return ctx
	}
	return context.WithValue(ctx, missionCtxKey{}, missionID)
}

// MissionIDFromContext returns the mission id bound by WithMissionID, or "" when
// the caller is not on a mission.
func MissionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(missionCtxKey{}).(string)
	return id
}

// MissionStore is the narrow slice of missionservice.Service these tools
// actually need: the four write verbs a unit reaches back through — file a
// report, revise the plan, come to rest with a verdict — plus liveness. It is
// deliberately NOT the full Service: a unit's back-channel has no business
// creating, listing, binding, or deleting missions (nor reading another's), so
// accepting this narrow slice keeps the seam honest and lets a composition point
// hand these tools exactly the authority a dispatched unit is meant to hold and
// nothing more. Every method the runtime's missionservice already implements;
// no test double re-implements it, because the tests exercise the real
// sqlite-backed store (the drift a fake would hide — SetPlan's shape validation,
// Finish's terminal guard — is exactly what these tools must surface faithfully).
type MissionStore interface {
	AddReport(ctx context.Context, missionID string, report *missionservice.Report) error
	Heartbeat(ctx context.Context, id string, lastErr string) (*missionservice.Mission, error)
	// SetPlan replaces the mission's plan with a full snapshot and returns the
	// stored mission, whose Plan carries the ids SetPlan assigned to id-less
	// entries — what mission_plan echoes so the next revision can carry ids
	// forward.
	SetPlan(ctx context.Context, id string, entries []missionservice.PlanEntry, explanation string) (*missionservice.Mission, error)
	// Finish moves the mission into a terminal state (guarded and immutable) and
	// returns the stored mission.
	Finish(ctx context.Context, id string, status missionservice.Status, reason string) (*missionservice.Mission, error)
}

// AttentionAsker is the durable-ask machinery mission_ask_attention hands an
// attention request to. It is deliberately a one-method seam so this package
// does not depend on hitlservice directly: the composition point (serve /
// `contenox acp`) wires it to the SAME durable ask store the operator inbox
// already reads (fleet-consolidation.md slices C1/C2), which is the "no second
// mechanism" invariant made concrete. When it is nil, mission_ask_attention
// degrades to filing a durable blocker report (see New).
type AttentionAsker interface {
	// RaiseAttention puts the unit's question to a human and BLOCKS until it
	// is answered, returning the operator's own words. An error means no
	// answer is coming (nobody replied inside the deadline, the reply was a
	// refusal, or the ask could not be recorded) — the caller's fallback, not
	// a failure to surface. The one exception: when ask.ParkWindow elapsed
	// unanswered, the error is a *taskengine.ApprovalPendingError and the
	// caller's job is to let the run SUSPEND (checkpoint-and-release) rather
	// than file a blocker — the question is still pending and answerable.
	//
	// It returns the answer rather than just an error because that is the whole
	// point of asking: a unit that learns "someone was notified" still cannot
	// proceed, while a unit handed "the contenox runtime repo, /home/x/src/…"
	// finishes its mission on the next turn.
	RaiseAttention(ctx context.Context, ask AttentionAsk) (string, error)
}

// AttentionAsk is one unit's question, plus the identity/parking knobs the
// checkpoint-and-release path needs. Zero values of AskID/ParkWindow mean the
// pre-detach behavior: fresh row ID, block to the ceiling.
type AttentionAsk struct {
	MissionID string
	Summary   string
	Detail    string
	// AskID is the durable row identity — the engine tool-call ID on the
	// suspendable path, so "ask ID == tool-call ID == checkpoint key" holds
	// for questions exactly as it does for permission asks.
	AskID string
	// ParkWindow, when > 0, bounds the block; past it the asker returns the
	// typed pending error described on RaiseAttention.
	ParkWindow time.Duration
}

type provider struct {
	missions MissionStore
	asker    AttentionAsker
	// The SUPERVISOR half (see supervisor.go), optional: a process that wires
	// neither leaves a firing session exactly as it was — with no mission tools at
	// all — rather than offering tools that cannot work.
	supervisor SupervisorStore
	resolver   AttentionResolver
	// parkWindow bounds a suspendable ask's block before checkpoint-and-release
	// (AttentionParkWindow unless overridden via WithAttentionParkWindow).
	parkWindow time.Duration
	// recordDowngrade is the verification gate's telemetry hook (see
	// WithDowngradeRecorder). Never nil: New installs a no-op, so the gate
	// bumps it unconditionally.
	recordDowngrade func()
}

// Option configures the provider at construction (the house's functional-option
// idiom), so the supervisor surface could be added without changing New's
// signature for the callers that only ever wanted the unit's own tools.
type Option func(*provider)

// WithSupervision wires the tools a session that FIRED missions gets: list your
// missions, answer a unit's question. Both deps are required together — listing
// without answering is a dead end, answering without listing gives the model no
// way to learn an ask id.
// WithAttentionParkWindow overrides the park duration (AttentionParkWindow by
// default) — tests shrink it so a suspension happens in milliseconds.
func WithAttentionParkWindow(d time.Duration) Option {
	return func(p *provider) {
		if d > 0 {
			p.parkWindow = d
		}
	}
}

func WithSupervision(store SupervisorStore, resolver AttentionResolver) Option {
	return func(p *provider) {
		p.supervisor = store
		p.resolver = resolver
	}
}

// New returns the mission-tools provider. reporter is required (a mission tool
// with no mission store is a wiring defect, not a runtime condition, so New
// panics rather than degrading — it is called once at composition). asker is
// OPTIONAL: when nil, mission_ask_attention files a durable BLOCKER report
// instead of a durable ask. That fallback is a deliberate, documented seam, not
// an oversight — the full unattended-ask delivery (a permission ask from a
// viewer-less unit landing in the operator inbox) is a separate prerequisite
// slice (fleet-consolidation.md M5); until its wiring reaches this composition
// point, "this unit needs attention" is still visible where an operator already
// looks — the mission's own reports — rather than being silently dropped.
func New(missions MissionStore, asker AttentionAsker, opts ...Option) taskengine.ToolsRepo {
	if missions == nil {
		panic("missiontools: mission store is required")
	}
	p := &provider{missions: missions, asker: asker, recordDowngrade: func() {}, parkWindow: AttentionParkWindow}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Supports always reports the single provider name. Exposure of the TOOLS is
// gated (on the mission id) in GetToolsForToolsByName, not here: the aggregate
// tools repo lists provider names from its own map and routes a deterministic
// `tools` Exec by name, so gating the provider's mere existence would break the
// deterministic path while adding nothing the tool-list gate does not already
// enforce for the model-driven path.
func (p *provider) Supports(context.Context) ([]string, error) {
	return []string{ToolsProviderName}, nil
}

// GetSchemasForSupportedTools has no OpenAPI schema surface (mirrors echo/print).
func (p *provider) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

// GetToolsForToolsByName lists the mission tools — but ONLY when the caller is
// on a mission. Off a mission it returns an empty slice, so the tools are ABSENT
// from a model's tool list rather than present-and-refused: the envelope
// enforced at construction (see the package doc). An unknown provider name is an
// error, as with every other provider.
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
	// Not a mission — but possibly a session that FIRED some. A supervisor gets a
	// different, smaller surface: see what you dispatched, answer what it asks.
	// Both gates are context facts, so neither set can leak into a session that is
	// neither (an ordinary chat sees no mission tools at all).
	if p.supervisor != nil && p.resolver != nil && ParentSessionIDFromContext(ctx) != "" {
		return supervisorTools(), nil
	}
	return []taskengine.Tool{}, nil
}

// Exec runs one mission-tool call. It refuses off-mission (the backstop for the
// deterministic `tools` path, which never consulted GetToolsForToolsByName), and
// routes on the tool name. A successful report or ask ALSO stamps a heartbeat:
// filing anything is proof of life, so the liveness signal rides the same call
// rather than requiring a separate cadence — the documented heartbeat trigger is
// "meaningful unit activity", and a mission tool call is exactly that.
func (p *provider) Exec(ctx context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if call == nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: missing tools call")
	}
	// The SUPERVISOR tools are authorized by the calling session having fired
	// missions, not by it being one — checked first so a firing session's call is
	// never rejected by the unit-only gate below.
	switch call.ToolName {
	case ToolNameListMissions, ToolNameAnswer:
		parentSessionID := ParentSessionIDFromContext(ctx)
		if parentSessionID == "" {
			return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %s is only available to a session that fired missions", call.ToolName)
		}
		if call.ToolName == ToolNameListMissions {
			return p.execListMissions(ctx, parentSessionID)
		}
		return p.execAnswer(ctx, parentSessionID, input, call)
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
		// An unattended unit that files a report without naming its shape almost
		// always means "here is where I am": default to progress rather than
		// erroring on a missing enum. A malformed kind still fails loudly in
		// AddReport's validation below.
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
	// The conclusion verification gate (see verify.go): a RESULT whose claimed
	// artifacts include a positively missing local path is downgraded to
	// progress and annotated — before the write, so the durable row already
	// tells the truth. The report always lands, through the same AddReport
	// below, so routing and inbox behavior are untouched; and the tool's own
	// reply names the downgrade so the unit learns it too.
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

// appendWarning attaches the verification warning to a report's detail, on its
// own paragraph — the same shape withUnansweredNote uses, so an annotated
// detail always reads as the unit's words first, the runtime's note after.
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

	// RESUME path first: a run resumed after checkpoint-and-release carries the
	// operator's answer (or the recorded fact that nobody gave one) injected
	// under this call's ID — consume it instead of asking the same question
	// twice.
	if ans, ok := taskengine.AttentionAnswerFromContext(ctx, callID); ok {
		p.heartbeat(ctx, missionID)
		if ans.Answered && strings.TrimSpace(ans.Text) != "" {
			return ans.Text, taskengine.DataTypeString, nil
		}
		// Denied/expired on resume: the same blocker fallback an unanswered
		// blocking ask takes, so the question is durably preserved.
		detail = withUnansweredNote(detail, fmt.Errorf("the ask was resolved without an answer while the run was suspended"))
	} else if p.asker != nil {
		// The unit parks here while a human reads the question. Its heartbeat is
		// stamped BEFORE the wait, not after: asking is proof of life, and a unit
		// blocked on an operator for ten minutes must not look dead for ten
		// minutes.
		p.heartbeat(ctx, missionID)
		ask := AttentionAsk{MissionID: missionID, Summary: summary, Detail: detail}
		// Park-and-release only where a suspend can actually land: the
		// model-batch execution site (whose output is the ChatHistory a
		// checkpoint needs) with a durable saver installed. Everywhere else
		// the ask blocks to the ceiling, exactly as before.
		if callID != "" && taskengine.ToolCallSuspendable(ctx) && taskengine.HasCheckpointSaver(ctx) {
			ask.AskID = callID
			ask.ParkWindow = p.parkWindow
		}
		answer, err := p.asker.RaiseAttention(ctx, ask)
		if err == nil {
			p.heartbeat(ctx, missionID)
			// The answer IS the tool result: the model reads it as the reply to
			// the question it just asked and continues in the same turn.
			return answer, taskengine.DataTypeString, nil
		}
		var pending *taskengine.ApprovalPendingError
		if errors.As(err, &pending) {
			// The park window elapsed with the question still open. Hand the
			// typed error up UNwrapped: the engine suspends the run under this
			// ask's ID, and the answer arrives via resume injection above.
			return nil, taskengine.DataTypeAny, pending
		}
		// No answer is coming. Fall through to the blocker below rather than
		// failing the call: the question is worth more durably recorded than
		// returned as an error the model will paraphrase into prose.
		detail = withUnansweredNote(detail, err)
	}
	// Fallback: no answer channel is wired here, or nobody answered. Record the
	// need for attention as a durable blocker report so the question is not lost.
	// Same store, no parallel mechanism.
	report := &missionservice.Report{Kind: missionservice.ReportKindBlocker, Summary: summary, Detail: detail}
	if err := p.missions.AddReport(ctx, missionID, report); err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: record attention request: %w", err)
	}
	p.heartbeat(ctx, missionID)
	return "attention requested (recorded as blocker — no operator answered)", taskengine.DataTypeString, nil
}

// withUnansweredNote appends WHY the question fell back to a blocker, so the
// operator reading the report knows an answer was actually solicited and missed
// rather than never asked for.
func withUnansweredNote(detail string, err error) string {
	note := fmt.Sprintf("(the unit asked for an operator and got no answer: %v)", err)
	if strings.TrimSpace(detail) == "" {
		return note
	}
	return detail + "\n\n" + note
}

// execPlan replaces the caller's mission plan with a FULL SNAPSHOT and echoes
// the stored snapshot back to the model. The echo is the point: SetPlan assigns
// an id to every id-less entry and returns them on the mission it hands back, so
// returning that Plan (as JSON) is what lets the planner carry ids forward on the
// next revision — which is in turn what makes SetPlan's added/removed diff and
// its completed-work immutability guard key on stable identity rather than on
// content. The heartbeat rides the successful write like every other tool: a
// plan revision is meaningful unit activity, hence proof of life.
//
// Shape validation is missionservice's, not ours: an empty snapshot, an entry
// over the size cap, an unknown status/priority, or a rewrite of already-
// completed content all surface as SetPlan errors, which we wrap and hand back to
// the model verbatim so it can correct and retry. We stay HARD on parsing the
// arguments into the snapshot and SOFT on planning discipline (no "exactly one
// in_progress" rule here — that is the planner profile's prompt job).
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
	// Return the stored Plan as JSON: the model reads back the snapshot it just
	// wrote WITH the ids now assigned, and the transport's projection reads the
	// same JSON off the tool event to emit the ACP `plan` full-snapshot update.
	// One echo, two consumers, no parallel carrier.
	return m.Plan, taskengine.DataTypeJSON, nil
}

// execFinish brings the caller's mission to rest in a terminal state. Finish is
// the GUARDED path (see missionservice.Finish): the target must be terminal, a
// finished mission is immutable, a repeat of the same status is an idempotent
// no-op, and a different terminal status over an already-finished mission is a
// conflict — every one of which surfaces to the model as an error it can read.
// The heartbeat still rides the successful call: ending a mission is the last
// meaningful activity, and stamping it keeps "finished cleanly" distinguishable
// from "went dark right before finishing" in the liveness record.
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

// heartbeat stamps the mission's liveness on a successful tool call. It is
// best-effort: the report/ask has already committed, and if AddReport just
// succeeded the mission row exists, so a heartbeat error here is a transient
// storage hiccup that must not fail (or roll back) the tool the unit called. It
// is swallowed deliberately.
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
						"description": "The shape of the report: progress, finding, blocker, or result.",
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
			Description: "ASK your operator a question and WAIT for their reply — use it when you cannot proceed without a human: a decision you may not make unattended, a missing fact, an ambiguous instruction. The call BLOCKS until a person answers, and their answer comes back to you as this tool's result, so you continue with it on the same turn. If nobody answers in time your question is recorded as a blocker instead. Use it sparingly — it costs a human's attention and their time to reply — but prefer it over guessing or giving up.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{
						"type":        "string",
						"description": "A single-line summary of what you need the operator for.",
					},
					"detail": map[string]any{
						"type":        "string",
						"description": "Optional longer detail.",
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

// handoverArg is the wire shape a report's OPTIONAL typed hand-off arrives in: a
// JSON object under `handover`. It mirrors missionservice.Handover field for
// field; the tool stays hard on parsing shape and lets missionservice validate
// the caps and be soft on substance.
type handoverArg struct {
	Outcome         string   `json:"outcome"`
	Artifacts       []string `json:"artifacts"`
	HandoverForNext string   `json:"handoverForNext"`
	Caveats         string   `json:"caveats"`
}

// parseHandover reads the OPTIONAL `handover` object off a mission_report call,
// from either shape the call arrives in. The model-driven path passes it as a
// nested object under the map[string]any `input` (or, tolerantly, a JSON-encoded
// string); the deterministic `tools` task can only carry map[string]string Args,
// so there `handover` is necessarily a JSON string — the same two-shape handling
// parsePlanEntries does for `entries`. Absent, it returns (nil, nil) — a report
// with no hand-off is a legacy report, not an error. Present but MALFORMED, it
// returns an error the model can read and correct. An all-empty hand-off is left
// for missionservice.AddReport to collapse to nil, so this stays a pure parser.
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

// decodeHandover folds the model-shape `handover` value — a JSON object value, or
// a JSON-encoded string — into a typed handoverArg, the same fold decodePlanEntries
// does for entries: a raw string is unmarshalled directly, anything else is
// re-marshalled and re-parsed so the generic map[string]any the JSON decoder
// handed us becomes the typed shape without hand-walking maps.
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

// trimStrings drops empty/whitespace-only artifact refs, the same cleanup
// splitRefs applies to the deterministic refs shape.
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

// planEntryArg is the wire shape one plan entry arrives in from either call
// path: a JSON object under `entries`. Status and Priority are read as plain
// strings and cast to the missionservice enums, which SetPlan then validates —
// this tool stays hard on parsing shape and soft on discipline.
type planEntryArg struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// parsePlanEntries reads the full-snapshot `entries` list from either shape a
// mission-tool call arrives in. The model-driven path passes arguments as a
// map[string]any `input`, where `entries` is a JSON array (a []any of objects) —
// or, tolerantly, a JSON-encoded string. The deterministic `tools` task can only
// carry map[string]string Args, so there `entries` is necessarily a JSON string.
// Either way the entries are normalized to typed missionservice.PlanEntry values;
// SetPlan owns all further validation (empty snapshot, caps, unknown enums,
// completed-immutability), so a malformed plan fails loudly there rather than
// being silently coerced here.
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

// decodePlanEntries folds the model-shape `entries` value — a JSON array value,
// or a JSON-encoded string — into typed entry args. A raw string is unmarshalled
// directly; anything else is re-marshalled and re-parsed, which turns the generic
// []any/map[string]any the JSON decoder handed us into the typed shape without
// hand-walking maps.
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

// argString reads a string argument by key from either shape a tool call can
// arrive in: the model-driven path passes arguments as a map[string]any `input`;
// the deterministic `tools` task passes them as the call's map[string]string
// Args. The model shape wins when present.
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

// argStrings reads a string-list argument. From the model shape it accepts a
// JSON array (or a single string); from the deterministic Args it accepts a
// comma- or newline-separated value (map[string]string cannot carry a list).
// Empty entries are dropped.
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
