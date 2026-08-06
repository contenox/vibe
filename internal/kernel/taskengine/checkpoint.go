package taskengine

// This file defines the versioned checkpoint a suspended run is rebuilt
// from, the typed errors the suspend path travels on, and the context
// plumbing (saver, resume state, injected verdicts) the service layer wires
// around a run. taskengine defines the shape and wire format but never
// touches storage; a CheckpointSaver on the run context owns persistence.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// CheckpointSchemaVersion is the wire version MarshalCheckpoint writes. Bump
// it together with a migration entry in checkpointMigrations, never alone.
const CheckpointSchemaVersion = 1

// ErrCheckpointVersion reports a checkpoint whose schema version this binary
// cannot load (newer than it knows, or older with no migration registered).
var ErrCheckpointVersion = errors.New("taskengine: unsupported checkpoint schema version")

// checkpointMigrations upgrades a checkpoint payload from version N to N+1.
// A compile-time table, never mutated at runtime. UnmarshalCheckpoint
// applies entries sequentially until the payload reaches the current version.
var checkpointMigrations = map[int]func(raw []byte) ([]byte, error){}

// ApprovalPendingError is the HITL wrapper's third outcome beside allow/deny:
// the fast-path park elapsed with no human verdict. The durable approval row
// exists before this is returned; the executor persists a checkpoint and
// releases the run after.
type ApprovalPendingError struct {
	// ApprovalID is the durable approval row's ID and the checkpoint key.
	ApprovalID string
	ToolName   string
}

func (e *ApprovalPendingError) Error() string {
	return fmt.Sprintf("tool call %q is awaiting human approval (approval %s); the run suspends until the approval is answered", e.ToolName, e.ApprovalID)
}

// ChainSuspendedError is ExecEnv's terminal for a suspended run: the
// checkpoint is persisted and answering the approval resumes the chain. It
// is a typed outcome, not a failure.
type ChainSuspendedError struct {
	ApprovalID string
	// Scope is the hierarchical address of the interrupt point.
	Scope EventScope
}

func (e *ChainSuspendedError) Error() string {
	return fmt.Sprintf("chain %s suspended at task %s awaiting approval %s; answer the approval to resume the run", e.Scope.Chain, e.Scope.Task, e.ApprovalID)
}

// PendingToolCall records one model-requested tool call the suspended run
// has not answered yet: the awaiting-verdict call first, then any calls of
// the batch that never started.
type PendingToolCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Checkpoint is everything a suspended run needs to resume in any process:
// position, state (vars, edge counts, chat history), pending tool calls, and
// the request-scoped identity the service layer re-injects.
//
// A checkpoint is keyed by the one call awaiting a verdict; PendingCalls
// also lists the batch's not-yet-started calls. Resume injects that one
// verdict and re-enters execute_tool_calls, which runs the remaining calls
// through the normal path — each may gate again and suspend afresh under
// its own key.
type Checkpoint struct {
	ApprovalID   string
	PendingCalls []PendingToolCall
	// Chain is the full definition as executed: chains are data, so a
	// reference could dangle across a restart, but the definition cannot.
	Chain      *TaskChainDefinition
	TaskID     string
	RetryIndex int
	Scope      EventScope
	// Vars/VarTypes round-trip through the closed DataType enum (see
	// decodeCheckpointVar) — no reflection.
	Vars       map[string]any
	VarTypes   map[string]DataType
	EdgeCounts map[string]int
	// History ends with the unanswered tool calls (plus any results
	// produced before the gate) — the shape the tool-pairing repair path
	// re-enters on.
	History           ChatHistory
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

// checkpointVarWire is one typed variable on the wire: the DataType name
// plus the value's raw JSON.
type checkpointVarWire struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// checkpointWireV1 is schema_version 1. Additive fields are fine within a
// version; any change of meaning bumps CheckpointSchemaVersion with a migration.
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

// MarshalCheckpoint encodes cp as the current schema version. A var that
// cannot be marshalled fails the whole encode, rather than resuming a
// different run than the one that suspended.
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

// UnmarshalCheckpoint decodes raw, migrating older schema versions forward.
// A version this binary cannot reach errors with ErrCheckpointVersion rather
// than risking a mis-decoded, silently corrupted resume.
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
// checkpointMigrations, one step at a time.
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

// decodeCheckpointVar materializes a var through the closed DataType enum,
// so e.g. a chat_history var decodes as ChatHistory, not a map.
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
// installs an implementation on the run context; Save must be atomic per
// call — either the checkpoint is readable under its approval ID afterward,
// or the run fails instead of suspending into a lost run.
type CheckpointSaver interface {
	SaveCheckpoint(ctx context.Context, cp *Checkpoint) error
}

type checkpointSaverContextKey struct{}

// WithCheckpointSaver installs the durable checkpoint sink for runs under
// ctx. Without one, a run that hits an approval park fails with a teaching
// error instead of suspending, never a silent loss.
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

// HasCheckpointSaver reports whether a durable checkpoint sink is installed
// on this run — the precondition for any park-and-release ask.
func HasCheckpointSaver(ctx context.Context) bool {
	return checkpointSaverFromContext(ctx) != nil
}

type approvalVerdictsContextKey struct{}

// WithApprovalVerdicts pre-loads human verdicts keyed by approval ID for a
// resumed run: the HITL wrapper executes (true) or denies (false) a call
// whose verdict is already recorded, without gating it again. The map is copied.
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

// GateResult is the durably recorded output of a resumed run's approved gate
// call. A resume that executes the gated tool records its result before the
// chain continues (WithGateResultRecorder); a retry of that same run replays
// the record instead of executing again (WithRecordedGateResults) — the seam
// that keeps the approved call exactly-once across partially completed resumes.
type GateResult struct {
	Value any
	Type  DataType
}

// MarshalGateResult encodes r for storage alongside a checkpoint, in the same
// typed wire form as checkpoint vars.
func MarshalGateResult(r GateResult) ([]byte, error) {
	raw, err := json.Marshal(r.Value)
	if err != nil {
		return nil, fmt.Errorf("taskengine: gate result (%s) is not JSON-serializable: %w", r.Type.String(), err)
	}
	return json.Marshal(checkpointVarWire{Type: r.Type.String(), Value: raw})
}

// UnmarshalGateResult decodes bytes MarshalGateResult produced, materializing
// the value through the closed DataType enum exactly as checkpoint vars do.
func UnmarshalGateResult(raw []byte) (GateResult, error) {
	var wire checkpointVarWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return GateResult{}, fmt.Errorf("taskengine: gate result payload is not valid JSON: %w", err)
	}
	dt, err := DataTypeFromString(wire.Type)
	if err != nil {
		return GateResult{}, fmt.Errorf("taskengine: gate result: %w", err)
	}
	value, err := decodeCheckpointVar(dt, wire.Value)
	if err != nil {
		return GateResult{}, fmt.Errorf("taskengine: gate result: %w", err)
	}
	return GateResult{Value: value, Type: dt}, nil
}

type gateResultRecorderContextKey struct{}

// GateResultRecorder persists the approved gate call's result under its
// approval ID before the resumed chain continues past it. Installed only on
// the resume path; recording failure fails the call rather than letting an
// unrecorded side effect become repeatable on retry.
type GateResultRecorder func(ctx context.Context, approvalID string, result GateResult) error

// WithGateResultRecorder installs the durable gate-result sink for a resumed run.
func WithGateResultRecorder(ctx context.Context, rec GateResultRecorder) context.Context {
	if rec == nil {
		return ctx
	}
	return context.WithValue(ctx, gateResultRecorderContextKey{}, rec)
}

// GateResultRecorderFromContext reports the installed recorder, nil when none.
func GateResultRecorderFromContext(ctx context.Context) GateResultRecorder {
	rec, _ := ctx.Value(gateResultRecorderContextKey{}).(GateResultRecorder)
	return rec
}

// ErrGateRecordFailed marks a gated call that executed but whose
// exactly-once record could not be persisted. execute_tool_calls treats it as
// a hard task failure instead of the usual soft tool-error result: continuing
// would tell the model the call failed and invite a re-issue of work the
// world already saw.
var ErrGateRecordFailed = errors.New("taskengine: gated call executed but recording its result failed")

type recordedGateResultsContextKey struct{}

// GateResultStore is the mutable record/replay table a resumed run shares
// between its recorder and the HITL wrapper's replay lookup. Mutable on
// purpose: a record made during attempt N of a retrying task must replay in
// attempt N+1 of the same Execute call — the durable row is not re-read
// mid-run, so a snapshot map would leave same-process retries unprotected.
type GateResultStore struct {
	mu sync.Mutex
	m  map[string]GateResult
}

// NewGateResultStore returns an empty store.
func NewGateResultStore() *GateResultStore {
	return &GateResultStore{m: map[string]GateResult{}}
}

// Set records approvalID's completed result for replay.
func (s *GateResultStore) Set(approvalID string, r GateResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[approvalID] = r
}

// Get reports the recorded result for approvalID, ok=false when none.
func (s *GateResultStore) Get(approvalID string) (GateResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[approvalID]
	return r, ok
}

// WithGateResultStore installs the shared record/replay table for a resumed run.
func WithGateResultStore(ctx context.Context, store *GateResultStore) context.Context {
	if store == nil {
		return ctx
	}
	return context.WithValue(ctx, recordedGateResultsContextKey{}, store)
}

// RecordedGateResultFromContext reports the recorded result for approvalID,
// ok=false when none was recorded on this run.
func RecordedGateResultFromContext(ctx context.Context, approvalID string) (GateResult, bool) {
	if approvalID == "" {
		return GateResult{}, false
	}
	store, _ := ctx.Value(recordedGateResultsContextKey{}).(*GateResultStore)
	if store == nil {
		return GateResult{}, false
	}
	return store.Get(approvalID)
}

type attentionAnswersContextKey struct{}

// AttentionAnswer is the text twin of an approval verdict: the operator's
// words, or Answered=false meaning the asking tool runs its blocker fallback.
type AttentionAnswer struct {
	Answered bool
	Text     string
}

// WithAttentionAnswers pre-loads operator answers keyed by ask ID for a
// resumed run, exactly as WithApprovalVerdicts pre-loads verdicts. The map
// is copied.
func WithAttentionAnswers(ctx context.Context, answers map[string]AttentionAnswer) context.Context {
	if len(answers) == 0 {
		return ctx
	}
	copied := make(map[string]AttentionAnswer, len(answers))
	for k, v := range answers {
		copied[k] = v
	}
	return context.WithValue(ctx, attentionAnswersContextKey{}, copied)
}

// AttentionAnswerFromContext reports the pre-loaded answer for askID,
// ok=false when none was injected.
func AttentionAnswerFromContext(ctx context.Context, askID string) (ans AttentionAnswer, ok bool) {
	if askID == "" {
		return AttentionAnswer{}, false
	}
	answers, _ := ctx.Value(attentionAnswersContextKey{}).(map[string]AttentionAnswer)
	if answers == nil {
		return AttentionAnswer{}, false
	}
	ans, ok = answers[askID]
	return ans, ok
}

type suspendableToolCallContextKey struct{}

// WithSuspendableToolCall marks the current tool call as one the engine can
// suspend on. Set only at the model-batch execution site, whose task output
// is the ChatHistory suspendRun requires; a tools-handler call never carries
// it, so askers gate park-and-release behavior on this marker.
func WithSuspendableToolCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, suspendableToolCallContextKey{}, true)
}

// ToolCallSuspendable reports whether the current tool call may suspend the
// run — see WithSuspendableToolCall.
func ToolCallSuspendable(ctx context.Context) bool {
	ok, _ := ctx.Value(suspendableToolCallContextKey{}).(bool)
	return ok
}

type resumeCheckpointContextKey struct{}

// WithResumeCheckpoint marks the run under ctx as a resume of cp: ExecEnv
// restores vars/edgeCounts, re-enters at cp.TaskID, and feeds the
// checkpointed history verbatim into that task's first attempt.
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
// with a (possibly partially answered) tool-call batch. Empty when the
// transcript does not end in an open batch.
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
