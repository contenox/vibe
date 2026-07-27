package taskengine

// This file is the engine half of the S6 "durable envelopes" slice: the
// versioned checkpoint a suspended run is rebuilt from, the typed errors the
// suspend path travels on, and the context plumbing (saver, resume state,
// injected verdicts) the service layer wires around a run.
//
// Layering invariant: taskengine defines the checkpoint SHAPE and its wire
// format but never touches storage — a CheckpointSaver installed on the run
// context (agentservice) owns persistence, so the kernel stays below the
// store. The checkpoint is keyed by approval ID, which on execute_tool_calls
// paths EQUALS the engine-minted tool-call ID (scope.tool_call, see
// docs/development/engine-events.md §2) — a checkpoint joins back to its
// emitting position without string parsing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CheckpointSchemaVersion is the wire version MarshalCheckpoint writes.
// Bump it TOGETHER with a migration entry in checkpointMigrations — never
// alone. (Cautionary precedent, recorded in the eino decision memo: that
// project shipped checkpoints unversioned, silently dropped a pointer field
// on serialization, and corrupted resumed runs; versioning + per-field
// round-trip tests exist here from day one.)
const CheckpointSchemaVersion = 1

// ErrCheckpointVersion reports a checkpoint whose schema version this binary
// cannot load (newer than it knows, or older with no migration registered).
var ErrCheckpointVersion = errors.New("taskengine: unsupported checkpoint schema version")

// checkpointMigrations upgrades a checkpoint payload from version N to N+1.
// It is a compile-time table (never mutated at runtime — the never-do list
// bans mutable process-init registries): when CheckpointSchemaVersion becomes
// 2, an entry 1→2 lands here in the same commit. UnmarshalCheckpoint applies
// entries sequentially until the payload reaches the current version, so a
// checkpoint written by any released binary stays loadable.
var checkpointMigrations = map[int]func(raw []byte) ([]byte, error){}

// ApprovalPendingError is the HITL wrapper's third outcome beside allow/deny:
// the fast-path park (localtools.ApprovalParkWindow) elapsed with no human
// verdict. The durable approval row for ApprovalID already exists when this
// is returned (row first); the executor reacts by persisting a checkpoint
// (checkpoint second) and releasing the run (release third).
type ApprovalPendingError struct {
	// ApprovalID is the durable approval row's ID — equal to the engine-minted
	// tool-call ID on execute_tool_calls paths — and the checkpoint key.
	ApprovalID string
	// ToolName is the gated tool, for teaching messages.
	ToolName string
}

func (e *ApprovalPendingError) Error() string {
	return fmt.Sprintf("tool call %q is awaiting human approval (approval %s); the run suspends until the approval is answered", e.ToolName, e.ApprovalID)
}

// ChainSuspendedError is ExecEnv's terminal for a suspended run: the
// checkpoint is persisted, the durable approval row is pending, and answering
// the approval (hitlservice.Respond → the registered resume hook →
// agentservice.ResumeFromCheckpoint) continues the chain. It is a typed
// outcome, not a failure — callers that understand suspension (agentservice)
// translate it into a Suspended stop reason; a caller that does not at least
// surfaces a teaching message naming the approval to answer.
type ChainSuspendedError struct {
	// ApprovalID keys both the approval row and the checkpoint.
	ApprovalID string
	// Scope is the S5 hierarchical address of the interrupt point:
	// {chain, task, tool_call}.
	Scope EventScope
}

func (e *ChainSuspendedError) Error() string {
	return fmt.Sprintf("chain %s suspended at task %s awaiting approval %s; answer the approval to resume the run", e.Scope.Chain, e.Scope.Task, e.ApprovalID)
}

// PendingToolCall records one model-requested tool call the suspended run has
// not answered yet: the awaiting-verdict call first, then any calls of the
// batch that never started. IDs and arguments are recorded so an inbox can
// show what the run wants to do; the resume path itself re-derives the
// unanswered set from History (the transcript is authoritative).
type PendingToolCall struct {
	// CallID is the engine-minted unique call ID (== approval ID for the call
	// actually awaiting a verdict).
	CallID string `json:"call_id"`
	// Name is the namespaced tool name the model requested.
	Name string `json:"name"`
	// Arguments is the raw JSON argument string of the call.
	Arguments string `json:"arguments"`
}

// Checkpoint is everything a suspended run needs to resume in ANY process:
// position (chain + task + retry + S5 address), state (vars with their
// closed-enum types, edge counts, the full chat history including the
// partial results of the interrupted batch), the pending tool calls, and the
// request-scoped identity the service layer re-injects (template vars, tools
// allowlist, context length, session/mission, request ID).
//
// Composite-batch semantics (documented decision): a checkpoint is keyed by
// the ONE call awaiting a verdict; PendingCalls also lists the batch's
// not-yet-started calls. Resume injects that one verdict and re-enters
// execute_tool_calls, which runs the remaining calls through the normal
// path — each may gate again and suspend afresh under its own key. Verdicts
// are therefore collected sequentially, one suspension per gated call, which
// is the simple correct semantics for an engine that executes calls one at a
// time.
type Checkpoint struct {
	// ApprovalID keys the checkpoint — equal to the awaiting call's ID.
	ApprovalID string
	// PendingCalls: awaiting-verdict call first, then unstarted batch calls.
	PendingCalls []PendingToolCall
	// Chain is the full definition as executed (chains are data; a reference
	// could dangle across a restart, the definition cannot).
	Chain *TaskChainDefinition
	// TaskID + RetryIndex name the interrupted attempt; Scope is its S5
	// address including the tool call.
	TaskID     string
	RetryIndex int
	Scope      EventScope
	// Vars/VarTypes are the chain's variable state. Values round-trip through
	// the closed DataType enum (see decodeCheckpointVar) — no reflection.
	Vars     map[string]any
	VarTypes map[string]DataType
	// EdgeCounts preserves loop-bounding state (edge_traversed_at_least).
	EdgeCounts map[string]int
	// History is the full transcript at the interrupt point, ending with the
	// unanswered tool calls (plus any results the batch produced before the
	// gate) — the exact shape the tool-pairing repair path re-enters on.
	History ChatHistory
	// Request-scoped identity, re-injected on resume.
	TemplateVars      map[string]string
	ToolsAllowlist    []string
	HasToolsAllowlist bool
	ContextLength     int
	SessionID         string
	MissionID         string
	RequestID         string
	ChainRef          string
	CreatedAt         time.Time
}

// checkpointVarWire is one typed variable on the wire: the DataType name plus
// the value's raw JSON. Decoding goes back through the closed enum, so a
// ChatHistory-typed var materializes as ChatHistory again, not map[string]any.
type checkpointVarWire struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// checkpointWireV1 is schema_version 1. Evolution policy: additive fields are
// fine within a version; any change of meaning bumps CheckpointSchemaVersion
// with a migration.
type checkpointWireV1 struct {
	SchemaVersion     int                          `json:"schema_version"`
	ApprovalID        string                       `json:"approval_id"`
	PendingCalls      []PendingToolCall            `json:"pending_calls,omitempty"`
	Chain             *TaskChainDefinition         `json:"chain"`
	TaskID            string                       `json:"task_id"`
	RetryIndex        int                          `json:"retry_index"`
	Scope             EventScope                   `json:"scope"`
	Vars              map[string]checkpointVarWire `json:"vars,omitempty"`
	EdgeCounts        map[string]int               `json:"edge_counts,omitempty"`
	History           ChatHistory                  `json:"history"`
	TemplateVars      map[string]string            `json:"template_vars,omitempty"`
	ToolsAllowlist    []string                     `json:"tools_allowlist,omitempty"`
	HasToolsAllowlist bool                         `json:"has_tools_allowlist,omitempty"`
	ContextLength     int                          `json:"context_length,omitempty"`
	SessionID         string                       `json:"session_id,omitempty"`
	MissionID         string                       `json:"mission_id,omitempty"`
	RequestID         string                       `json:"request_id,omitempty"`
	ChainRef          string                       `json:"chain_ref,omitempty"`
	CreatedAt         time.Time                    `json:"created_at"`
}

// MarshalCheckpoint encodes cp as the current schema version. Every var is
// encoded with its declared type; a var that cannot be marshalled fails the
// whole encode with a teaching error (a checkpoint missing state would resume
// a different run than the one that suspended).
func MarshalCheckpoint(cp *Checkpoint) ([]byte, error) {
	if cp == nil {
		return nil, fmt.Errorf("taskengine: cannot marshal a nil checkpoint")
	}
	wire := checkpointWireV1{
		SchemaVersion:     CheckpointSchemaVersion,
		ApprovalID:        cp.ApprovalID,
		PendingCalls:      cp.PendingCalls,
		Chain:             cp.Chain,
		TaskID:            cp.TaskID,
		RetryIndex:        cp.RetryIndex,
		Scope:             cp.Scope,
		EdgeCounts:        cp.EdgeCounts,
		History:           cp.History,
		TemplateVars:      cp.TemplateVars,
		ToolsAllowlist:    cp.ToolsAllowlist,
		HasToolsAllowlist: cp.HasToolsAllowlist,
		ContextLength:     cp.ContextLength,
		SessionID:         cp.SessionID,
		MissionID:         cp.MissionID,
		RequestID:         cp.RequestID,
		ChainRef:          cp.ChainRef,
		CreatedAt:         cp.CreatedAt,
	}
	if len(cp.Vars) > 0 {
		wire.Vars = make(map[string]checkpointVarWire, len(cp.Vars))
		for name, value := range cp.Vars {
			dt, ok := cp.VarTypes[name]
			if !ok {
				dt = InferDataType(value)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("taskengine: checkpoint var %q (%s) is not JSON-serializable: %w", name, dt.String(), err)
			}
			wire.Vars[name] = checkpointVarWire{Type: dt.String(), Value: raw}
		}
	}
	return json.Marshal(wire)
}

// UnmarshalCheckpoint decodes raw, migrating older schema versions forward
// through checkpointMigrations. A version this binary cannot reach errors
// with ErrCheckpointVersion rather than guessing — a mis-decoded checkpoint
// would silently resume corrupted state, the exact failure the versioned
// envelope exists to prevent.
func UnmarshalCheckpoint(raw []byte) (*Checkpoint, error) {
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("taskengine: checkpoint payload is not valid JSON: %w", err)
	}
	version := probe.SchemaVersion
	if version <= 0 {
		return nil, fmt.Errorf("%w: payload declares no schema_version", ErrCheckpointVersion)
	}
	raw, version, err := migrateCheckpointPayload(raw, version, CheckpointSchemaVersion)
	if err != nil {
		return nil, err
	}

	var wire checkpointWireV1
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("taskengine: decode checkpoint v%d: %w", version, err)
	}
	cp := &Checkpoint{
		ApprovalID:        wire.ApprovalID,
		PendingCalls:      wire.PendingCalls,
		Chain:             wire.Chain,
		TaskID:            wire.TaskID,
		RetryIndex:        wire.RetryIndex,
		Scope:             wire.Scope,
		EdgeCounts:        wire.EdgeCounts,
		History:           wire.History,
		TemplateVars:      wire.TemplateVars,
		ToolsAllowlist:    wire.ToolsAllowlist,
		HasToolsAllowlist: wire.HasToolsAllowlist,
		ContextLength:     wire.ContextLength,
		SessionID:         wire.SessionID,
		MissionID:         wire.MissionID,
		RequestID:         wire.RequestID,
		ChainRef:          wire.ChainRef,
		CreatedAt:         wire.CreatedAt,
	}
	if len(wire.Vars) > 0 {
		cp.Vars = make(map[string]any, len(wire.Vars))
		cp.VarTypes = make(map[string]DataType, len(wire.Vars))
		for name, v := range wire.Vars {
			dt, err := DataTypeFromString(v.Type)
			if err != nil {
				return nil, fmt.Errorf("taskengine: checkpoint var %q: %w", name, err)
			}
			value, err := decodeCheckpointVar(dt, v.Value)
			if err != nil {
				return nil, fmt.Errorf("taskengine: checkpoint var %q: %w", name, err)
			}
			cp.Vars[name] = value
			cp.VarTypes[name] = dt
		}
	}
	return cp, nil
}

// migrateCheckpointPayload walks raw from version up to target through
// checkpointMigrations, one step at a time. It is the whole migration engine,
// separated from UnmarshalCheckpoint so the mechanism stays testable while
// only one live version exists.
func migrateCheckpointPayload(raw []byte, version, target int) ([]byte, int, error) {
	for version < target {
		migrate, ok := checkpointMigrations[version]
		if !ok {
			return nil, version, fmt.Errorf("%w: no migration from version %d toward %d", ErrCheckpointVersion, version, target)
		}
		migrated, err := migrate(raw)
		if err != nil {
			return nil, version, fmt.Errorf("taskengine: checkpoint migration from version %d failed: %w", version, err)
		}
		raw = migrated
		version++
	}
	if version > target {
		return nil, version, fmt.Errorf("%w: payload is version %d but this binary knows only %d — update the binary to resume this run", ErrCheckpointVersion, version, target)
	}
	return raw, version, nil
}

// decodeCheckpointVar materializes a var through the closed DataType enum —
// the "typed state" invariant: a chat_history var decodes as ChatHistory, not
// as a map, so a resumed run's handlers receive the same Go types the
// suspended run held. No reflect, no open registry.
func decodeCheckpointVar(dt DataType, raw json.RawMessage) (any, error) {
	switch dt {
	case DataTypeNil:
		return nil, nil
	case DataTypeString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("declared string but does not decode as one: %w", err)
		}
		return s, nil
	case DataTypeInt:
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("declared int but does not decode as one: %w", err)
		}
		return n, nil
	case DataTypeChatHistory:
		var h ChatHistory
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, fmt.Errorf("declared chat_history but does not decode as one: %w", err)
		}
		return h, nil
	case DataTypeJSON, DataTypeAny:
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("does not decode as JSON: %w", err)
		}
		return v, nil
	default:
		return nil, fmt.Errorf("unknown data type %d", int(dt))
	}
}

// CheckpointSaver persists a suspension checkpoint. The service layer
// (agentservice) installs an implementation on the run context; the executor
// calls it BEFORE returning ChainSuspendedError, so by the time the run is
// released the checkpoint is durable (row first, checkpoint second, release
// third). Save must be atomic-per-call: either the checkpoint is readable
// under its approval ID afterwards, or an error is returned and the run FAILS
// instead of suspending — a suspension without a checkpoint is a lost run.
type CheckpointSaver interface {
	SaveCheckpoint(ctx context.Context, cp *Checkpoint) error
}

type checkpointSaverContextKey struct{}

// WithCheckpointSaver installs the durable checkpoint sink for runs under ctx.
// Without one, a run that hits an approval park cannot suspend durably and
// fails with a teaching error instead (the approval row stays pending for the
// sweeper) — never a silent loss.
func WithCheckpointSaver(ctx context.Context, saver CheckpointSaver) context.Context {
	if saver == nil {
		return ctx
	}
	return context.WithValue(ctx, checkpointSaverContextKey{}, saver)
}

func checkpointSaverFromContext(ctx context.Context) CheckpointSaver {
	saver, _ := ctx.Value(checkpointSaverContextKey{}).(CheckpointSaver)
	return saver
}

type approvalVerdictsContextKey struct{}

// WithApprovalVerdicts pre-loads human verdicts keyed by approval ID (== the
// engine-minted tool-call ID) for a resumed run. The HITL wrapper consults
// this map before asking: a call whose verdict is already recorded executes
// (true) or receives the standard deny message (false) without gating a
// second time. The map is copied; callers cannot mutate verdicts mid-run.
func WithApprovalVerdicts(ctx context.Context, verdicts map[string]bool) context.Context {
	if len(verdicts) == 0 {
		return ctx
	}
	copied := make(map[string]bool, len(verdicts))
	for k, v := range verdicts {
		copied[k] = v
	}
	return context.WithValue(ctx, approvalVerdictsContextKey{}, copied)
}

// ApprovalVerdictFromContext reports the pre-loaded verdict for approvalID,
// ok=false when none was injected.
func ApprovalVerdictFromContext(ctx context.Context, approvalID string) (approved bool, ok bool) {
	if approvalID == "" {
		return false, false
	}
	verdicts, _ := ctx.Value(approvalVerdictsContextKey{}).(map[string]bool)
	if verdicts == nil {
		return false, false
	}
	approved, ok = verdicts[approvalID]
	return approved, ok
}

type resumeCheckpointContextKey struct{}

// WithResumeCheckpoint marks the run under ctx as a RESUME of cp: ExecEnv
// restores vars/varTypes/edgeCounts, re-enters at cp.TaskID, and feeds the
// checkpointed history verbatim into that task's first attempt (no input_var
// redirection, no prompt-template render — the pending tool calls live in
// that transcript). Execution from there on is the normal path, including the
// tool-pairing repair machinery the re-entered execute_tool_calls task runs.
func WithResumeCheckpoint(ctx context.Context, cp *Checkpoint) context.Context {
	if cp == nil {
		return ctx
	}
	return context.WithValue(ctx, resumeCheckpointContextKey{}, cp)
}

func resumeCheckpointFromContext(ctx context.Context) *Checkpoint {
	cp, _ := ctx.Value(resumeCheckpointContextKey{}).(*Checkpoint)
	return cp
}

// unansweredToolCalls derives the pending set from a transcript that ends
// with a (possibly partially answered) tool-call batch — the same shape
// resumableToolCallBatch locates for execution. Empty when the transcript
// does not end in an open batch.
func unansweredToolCalls(msgs []Message) []PendingToolCall {
	idx, answered := resumableToolCallBatch(msgs)
	if idx < 0 {
		return nil
	}
	var out []PendingToolCall
	for _, tc := range msgs[idx].CallTools {
		if tc.ID == "" || answered[tc.ID] {
			continue
		}
		out = append(out, PendingToolCall{
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return out
}
