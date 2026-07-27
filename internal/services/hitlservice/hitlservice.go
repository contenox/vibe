// Package hitlservice evaluates approval policies for tool calls.
// Policy decisions (allow / deny / approve) are returned to the caller; the
// caller (typically a ToolsRepo decorator like localtools.HITLWrapper) is
// responsible for actually pausing execution and sourcing the human decision.
package hitlservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"

	libdb "github.com/contenox/beam/internal/libdbexec"
)

type KVReader interface {
	GetKV(ctx context.Context, key string, out interface{}) error
}

type PolicyEvaluator interface {
	Evaluate(ctx context.Context, toolsName, toolName string, args map[string]any) (EvaluationResult, error)
}

// ComputeBoundsReader loads the COMPUTE half of an envelope by policy name — the
// per-mission compute ceiling a fleet enforcement seam holds a mission under (see
// ComputeBounds). It is an OPTIONAL capability, deliberately NOT folded into the
// Service interface: a consumer reaches it by type assertion (the SessionAgentText
// precedent in agentinstance), so a Service double that predates compute bounds
// keeps satisfying Service without gaining a method, and a caller handed a Service
// that does not implement this treats every mission as unbounded — today's
// behavior. The concrete service here implements it.
type ComputeBoundsReader interface {
	ComputeBoundsFor(ctx context.Context, policyName string) (ComputeBounds, error)
}

// PolicyValidator reports whether a named HITL policy exists and loads. It is the
// CREATION-time existence check for a mission envelope — the opposite stance from
// ComputeBoundsFor's runtime fail-to-unbounded: validate strictly when a mission
// is created (refuse a nonexistent envelope up front) so a typo cannot silently
// substitute the default gate, but stay resilient once a mission is already
// running. It needs only a PolicySource, so both serve and the in-process editor
// can validate without constructing a full approval Service.
type PolicyValidator interface {
	ValidatePolicy(ctx context.Context, policyName string) error
}

// NewPolicyValidator returns a PolicyValidator over src. An empty policy name
// resolves through the same fallback chain Evaluate uses (fallbackPolicy, then
// the built-in default), so "which envelope" means the same thing to a
// creation-time check as it does at evaluation time.
func NewPolicyValidator(src PolicySource, tenantID, fallbackPolicy string) PolicyValidator {
	return &policyValidator{src: src, tenantID: tenantID, fallback: fallbackPolicy}
}

type policyValidator struct {
	src      PolicySource
	tenantID string
	fallback string
}

func (v *policyValidator) ValidatePolicy(ctx context.Context, policyName string) error {
	name := strings.TrimSpace(policyName)
	if name == "" {
		name = v.fallback
	}
	if name == "" {
		name = defaultPolicyName
	}
	if _, err := loadPolicy(ctx, v.src, v.tenantID, name); err != nil {
		return err
	}
	return nil
}

// approvalStore is the durable persistence surface RequestApproval, Respond,
// SweepExpired, and ListPending need: create a pending row, look it up,
// compare-and-swap it into a terminal state, and list rows by state. It is a
// strict subset of runtimetypes.Store (see runtime/runtimetypes/hitl_approvals.go)
// — declared here so hitlservice depends on only the methods it calls, not the
// whole Store.
//
// service.store (the KVReader constructor argument) is type-asserted against
// this interface at construction time. Every production caller
// (contenoxcli/serve_cmd.go, enginesvc.buildTools) already passes a
// runtimetypes.Store there, so it is satisfied automatically; callers that
// pass a bare KVReader fake for Evaluate()-only use (agentview, the /files
// policy filter, most of this package's own policy_test.go-style tests)
// simply never exercise RequestApproval/Respond/SweepExpired/ListPending —
// see the nil checks at the top of each.
type approvalStore interface {
	CreateHITLApproval(ctx context.Context, a *runtimetypes.HITLApproval) error
	GetHITLApproval(ctx context.Context, id string) (*runtimetypes.HITLApproval, error)
	ResolveHITLApproval(ctx context.Context, id string, state runtimetypes.HITLApprovalState, resolution json.RawMessage, resolvedAt time.Time) error
	ListExpiredHITLApprovals(ctx context.Context, asOf time.Time, limit int) ([]*runtimetypes.HITLApproval, error)
	ListHITLApprovals(ctx context.Context, state runtimetypes.HITLApprovalState, createdAtCursor *time.Time, limit int) ([]*runtimetypes.HITLApproval, error)
	ListHITLApprovalsForMission(ctx context.Context, missionID string, limit int) ([]*runtimetypes.HITLApproval, error)
}

type Service interface {
	PolicyEvaluator

	// RequestApproval durably records req as a pending ask, publishes the
	// approval_requested TaskEvent (unchanged shape/consumers), then blocks
	// until Respond answers it or the wait is bounded away (see
	// DefaultApprovalCeiling / SetApprovalCeiling): a matched rule's own
	// TimeoutS wins when set, otherwise the serve-level ceiling applies so an
	// ask nobody answers cannot block forever.
	RequestApproval(ctx context.Context, req ApprovalRequest, sink taskengine.TaskEventSink) (bool, error)

	// Respond transitions a pending approval to approved/denied and wakes any
	// requester parked on it in this process. It returns ErrApprovalNotFound,
	// ErrApprovalAlreadyResolved, or ErrApprovalExpired instead of a bare
	// false when approvalID cannot be answered — never a silent no-op.
	Respond(ctx context.Context, approvalID string, approved bool) error

	// RequestAttention durably records a unit's QUESTION for a human and blocks
	// until Answer replies with text, the ceiling expires it, or ctx ends. It is
	// the second ask kind this store carries: a permission ask is answered
	// yes/no, an attention ask is answered with data ("which project did you
	// mean?"), and the asking unit receives that data as its tool result. See
	// attention.go.
	RequestAttention(ctx context.Context, req AttentionRequest, sink taskengine.TaskEventSink) (string, error)

	// Answer resolves a pending ATTENTION ask with the operator's own words and
	// wakes the unit parked on it. It rejects a permission ask (answered
	// approve/deny) and returns the same sentinels Respond does.
	Answer(ctx context.Context, askID, text string) error

	// AnswerAsAgent is Answer, recorded as answered by a supervising AGENT rather
	// than a human — the actor an envelope's agent-answer cap is counted on.
	AnswerAsAgent(ctx context.Context, askID, text string) error

	// PendingAttentionAsks returns a mission's unanswered questions, newest first.
	PendingAttentionAsks(ctx context.Context, missionID string) ([]*runtimetypes.HITLApproval, error)

	// AttentionBoundsFor reads an envelope's say over who may answer a unit's
	// question: whether the supervising agent may at all, and how often. The zero
	// bounds — human-only — are what an envelope that declares none, or one that
	// fails to load, both yield.
	AttentionBoundsFor(ctx context.Context, policyName string) (AttentionBounds, error)

	// AgentAnswerCount reports how many of a mission's questions an agent (not a
	// human) has answered — the durable counter behind the envelope's cap.
	AgentAnswerCount(ctx context.Context, missionID string) (int, error)

	// SweepExpired resolves every pending approval whose deadline has
	// passed, applying its stored OnTimeout (default deny) exactly as
	// localtools.HITLWrapper's own on-timeout branch would. It is the
	// durability backstop for asks whose original requester is gone (a
	// process restart) — serve runs it on an interval; tests can call it
	// directly. Returns the number of rows it resolved.
	SweepExpired(ctx context.Context) (int, error)

	// ListPending is the read half of the durable ask C1 introduced: an ask
	// nobody can see is not answerable (fleet-consolidation.md slice C2,
	// defects D4/D5). It returns pending approvals newest first, bounded by
	// limit (the store's own MAXLIMIT when limit<=0; runtimetypes.
	// ErrLimitParamExceeded when limit is set above it), and always a
	// non-nil slice — a fleet with nothing pending must render empty, not
	// fail. Every field an operator needs to decide rides along on the
	// returned *runtimetypes.HITLApproval rows themselves (tool, args
	// summary, diff, policy name, and matched rule), since ListPending
	// hands back the durable row unprojected rather than a narrower DTO.
	ListPending(ctx context.Context, limit int) ([]*runtimetypes.HITLApproval, error)

	// AbandonMissionAsks closes every pending ask filed under missionID,
	// deliberately WITHOUT running the resume hook: it exists for `mission
	// stop`, where nothing of the stopped mission may continue. Each row
	// resolves to denied (an attention ask thereby reads as unanswered) and
	// the closed IDs are returned so the caller can delete any checkpoints
	// suspended under them.
	AbandonMissionAsks(ctx context.Context, missionID string) ([]string, error)
}

// Sentinel errors Respond returns instead of a silent false — see Respond's
// doc and the Service interface doc above.
var (
	// ErrApprovalNotFound reports that approvalID does not exist in the store.
	ErrApprovalNotFound = errors.New("hitlservice: approval not found")
	// ErrApprovalAlreadyResolved reports that approvalID was already answered
	// (approved or denied) by an earlier Respond call.
	ErrApprovalAlreadyResolved = errors.New("hitlservice: approval already answered")
	// ErrApprovalExpired reports that approvalID's deadline passed and the
	// sweeper already resolved it via OnTimeout before this Respond landed.
	ErrApprovalExpired = errors.New("hitlservice: approval expired before it was answered")
	// ErrNoCheckpoint is the resume hook's clean "nothing was suspended under
	// this approval" answer: the fast path answered it in-session, or the gate
	// never checkpointed. Respond treats it as a no-op, NOT a failure — the
	// verdict is durably recorded either way.
	ErrNoCheckpoint = errors.New("hitlservice: no suspended run is checkpointed under this approval")
)

// ResumeHook resumes the suspended run checkpointed under approvalID after
// its verdict landed with nobody parked on it — the S6 released case. The
// composition root registers one via SetResumeHook (agentservice.ResumeHook
// is the canonical implementation); hitlservice deliberately does NOT import
// agentservice, so the dependency points from the root downward.
//
// Contract: return ErrNoCheckpoint when no run is suspended under the ID (a
// clean no-op); any other error is surfaced by Respond WITH the note that the
// verdict itself was recorded — a failed resume never un-answers an approval,
// and the checkpoint is retained with a failure annotation for a retry.
type ResumeHook func(ctx context.Context, approvalID string) error

// ApprovalRecorder is the durable half the HITL wrapper's suspend path needs
// (S6): create the pending row BEFORE parking (row first — a restart between
// park and verdict must still show the ask), and resolve it inline when the
// fast path gets its verdict — WITHOUT triggering the resume hook, because
// the resolver IS the waiter in that case. It is satisfied by this package's
// service; the wrapper type-asserts its PolicyEvaluator against it, so
// evaluator-only fakes simply keep the pre-S6 blocking behavior.
type ApprovalRecorder interface {
	RecordPendingApproval(ctx context.Context, approvalID string, req ApprovalRequest) error
	ResolveApprovalInline(ctx context.Context, approvalID string, approved bool) error
}

const defaultPolicyName = "hitl-policy-default.json"

// DefaultApprovalCeiling bounds RequestApproval when the matched policy rule
// sets no TimeoutS of its own (Rule.TimeoutS == 0 today means "block
// indefinitely", see policy.go:86-87 — that is the unbounded-hang defect
// this ceiling repairs). An ask nobody answered within an hour is abandoned,
// and a late denial beats an eternal block. Override per-service via
// SetApprovalCeiling — contenoxcli/serve_cmd.go wires it to the
// HITL_APPROVAL_TIMEOUT serve setting.
const DefaultApprovalCeiling = time.Hour

type service struct {
	src            PolicySource
	tenantID       string
	store          KVReader
	tracker        libtracker.ActivityTracker
	fallbackPolicy string
	approvals      approvalStore

	mu              sync.Mutex
	pending         map[string]chan answer
	approvalCeiling time.Duration
	resumeHook      ResumeHook
}

// New constructs a hitlservice bound to a tenant. The tenantID is forwarded to
// every policy lookup the service performs.
func New(src PolicySource, tenantID string, store KVReader, tracker libtracker.ActivityTracker) Service {
	return NewWithDefaultPolicy(src, tenantID, store, tracker, defaultPolicyName)
}

func NewWithDefaultPolicy(src PolicySource, tenantID string, store KVReader, tracker libtracker.ActivityTracker, fallbackPolicy string) Service {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	if strings.TrimSpace(fallbackPolicy) == "" {
		fallbackPolicy = defaultPolicyName
	}
	svc := &service{
		src:            src,
		tenantID:       tenantID,
		store:          store,
		tracker:        tracker,
		fallbackPolicy: fallbackPolicy,
		pending:        make(map[string]chan answer),
	}
	if as, ok := store.(approvalStore); ok {
		svc.approvals = as
	}
	return svc
}

// SetApprovalCeiling overrides the serve-level approval-wait ceiling
// (DefaultApprovalCeiling otherwise) on svc, when svc was constructed by
// New/NewWithDefaultPolicy in this package. A no-op for any other Service
// implementation and for ceiling <= 0, so callers can pass an
// unparsed/unset config value without an extra branch.
func SetApprovalCeiling(svc Service, ceiling time.Duration) {
	if ceiling <= 0 {
		return
	}
	if s, ok := svc.(*service); ok {
		s.mu.Lock()
		s.approvalCeiling = ceiling
		s.mu.Unlock()
	}
}

// SetResumeHook registers the resume-on-verdict hook (see ResumeHook) on svc,
// when svc was constructed by New/NewWithDefaultPolicy in this package. Same
// shape as SetApprovalCeiling: a no-op for other implementations, so callers
// need no type switch. Instance-scoped on purpose — no global registry.
func SetResumeHook(svc Service, hook ResumeHook) {
	if s, ok := svc.(*service); ok {
		s.mu.Lock()
		s.resumeHook = hook
		s.mu.Unlock()
	}
}

func (s *service) hook() ResumeHook {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumeHook
}

func (s *service) ceiling() time.Duration {
	s.mu.Lock()
	d := s.approvalCeiling
	s.mu.Unlock()
	if d <= 0 {
		return DefaultApprovalCeiling
	}
	return d
}

var _ Service = (*service)(nil)
var _ ComputeBoundsReader = (*service)(nil)

// ComputeBoundsFor implements ComputeBoundsReader: it loads policyName's envelope
// and returns its compute bounds, or the zero (unbounded) bounds when the policy
// declares none. It resolves the name through the SAME fallback chain Evaluate
// uses (an empty name → the service's configured fallback → the built-in default),
// so "which envelope" means the same thing to a compute-bound check as it does to
// an action check.
//
// A policy that fails to load returns the zero bounds AND the load error: the
// caller records the error and proceeds UNBOUNDED, exactly as Evaluate falls back
// to the built-in (bound-less) default policy on a load failure. A broken policy
// therefore LOSES its ceiling rather than gaining a phantom one — the additive,
// restrict-only invariant holds even in the failure path.
func (s *service) ComputeBoundsFor(ctx context.Context, policyName string) (ComputeBounds, error) {
	policyPath := strings.TrimSpace(policyName)
	if policyPath == "" {
		policyPath = s.fallbackPolicy
	}
	if policyPath == "" {
		policyPath = defaultPolicyName
	}
	p, err := loadPolicy(ctx, s.src, s.tenantID, policyPath)
	if err != nil {
		return ComputeBounds{}, fmt.Errorf("hitlservice: load compute bounds for %q: %w", policyPath, err)
	}
	if p.Compute == nil {
		return ComputeBounds{}, nil
	}
	return *p.Compute, nil
}

const kvPrefixHITLPolicy = "cli.hitl-policy-name"

// policyNameContextKey scopes an explicit per-request HITL policy name onto a
// context. A single hitlservice is shared across many callers — serve builds ONE
// behind every ACP WebSocket session — so per-session policy differentiation
// cannot live in service state. Instead each ACP prompt turn injects its
// session's resolved policy name into the turn context (see acpsvc/prompt.go),
// and Evaluate prefers it over the process-global cli.hitl-policy-name KV. A
// context WITHOUT this key is unchanged: single-session callers (the CLI,
// `contenox acp`, `contenox chat`) keep reading the global KV.
type policyNameContextKey struct{}

// WithPolicyName returns a context that pins HITL evaluation to policyName for
// this request only. An empty/whitespace policyName returns ctx unchanged, so a
// caller that resolves to "no override" (a defaulting session) leaves the
// existing global-KV/fallback chain intact.
func WithPolicyName(ctx context.Context, policyName string) context.Context {
	policyName = strings.TrimSpace(policyName)
	if policyName == "" {
		return ctx
	}
	return context.WithValue(ctx, policyNameContextKey{}, policyName)
}

// PolicyNameFromContext returns the context-scoped policy override WithPolicyName
// pinned, or "" when none was. It is the reader half of that pair, exported so a
// caller that BUILDS the context (the unattended-permission answerer, an ACP
// prompt turn) can assert which envelope a request is carrying without reaching
// into this package's internals — and so a PolicyEvaluator implementation other
// than this one can honor the same override.
func PolicyNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(policyNameContextKey{}).(string)
	return strings.TrimSpace(name)
}

func (s *service) readActivePolicyName(ctx context.Context) string {
	var val string
	if err := s.store.GetKV(ctx, kvPrefixHITLPolicy, &val); err != nil {
		return ""
	}
	return strings.TrimSpace(val)
}

func (s *service) Evaluate(ctx context.Context, toolsName, toolName string, args map[string]any) (EvaluationResult, error) {
	reportErr, reportChange, end := s.tracker.Start(ctx, "hitl", "evaluate", "toolsName", toolsName, "toolName", toolName)
	defer end()
	// A per-request context override (an ACP session's chosen policy) wins over
	// the process-global active-policy KV so concurrent sessions behind ONE shared
	// service gate independently. Absent an override, fall through the existing
	// global-KV -> constructor fallback -> built-in default chain (unchanged for
	// single-session CLI callers).
	policyPath := PolicyNameFromContext(ctx)
	if policyPath == "" {
		policyPath = s.readActivePolicyName(ctx)
	}
	if policyPath == "" {
		policyPath = s.fallbackPolicy
	}
	if policyPath == "" {
		policyPath = defaultPolicyName
	}
	p, err := loadPolicy(ctx, s.src, s.tenantID, policyPath)
	if err != nil {
		reportErr(fmt.Errorf("hitl: falling back to built-in default policy: %w", err))
		p = defaultPolicy()
	}
	reportChange("policy", policyPath)
	result := evaluate(p, toolsName, toolName, args)
	result.PolicyName = policyPath
	return result, nil
}

func (s *service) RequestApproval(ctx context.Context, req ApprovalRequest, sink taskengine.TaskEventSink) (bool, error) {
	if s.approvals == nil {
		return false, fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	// A caller-supplied ToolCallID IS the ask's durable identity: the child
	// wrapper files its pending row under the engine tool-call ID before the
	// ask ever crosses the ACP wire, so a parent-side RequestApproval carrying
	// that same ID must ADOPT the existing row, not mint a twin. (The twin was
	// the dual-inbox wart: answering the child's row worked, the parent's
	// duplicate sat pending until the sweeper expired it.)
	approvalID := req.ToolCallID
	adopted := false
	if approvalID == "" {
		approvalID = uuid.NewString()
	} else if row, err := s.approvals.GetHITLApproval(ctx, approvalID); err == nil {
		if row.State != runtimetypes.HITLApprovalPending {
			return row.State == runtimetypes.HITLApprovalApproved, nil
		}
		adopted = true
	}

	// A matched rule's own TimeoutS always wins. Absent one, the row (and the
	// wait below) is bounded by the serve-level ceiling instead of blocking
	// indefinitely — see DefaultApprovalCeiling.
	ruleTimeout := req.TimeoutS > 0
	timeoutDur := s.ceiling()
	if ruleTimeout {
		timeoutDur = time.Duration(req.TimeoutS) * time.Second
	}

	if !adopted {
		row := buildApprovalRow(approvalID, req, time.Now().UTC(), timeoutDur)
		// Durable pending row FIRST — a restart between here and the answer must
		// still show this ask as pending, not lose it (fleet-consolidation.md
		// slice C1, defect D3).
		if err := s.approvals.CreateHITLApproval(ctx, row); err != nil {
			// A create-vs-create race on a shared ToolCallID: exactly one
			// wins; the loser re-reads and adopts (or returns the verdict a
			// blink-fast resolver already recorded).
			if req.ToolCallID != "" {
				if row, getErr := s.approvals.GetHITLApproval(ctx, approvalID); getErr == nil {
					if row.State != runtimetypes.HITLApprovalPending {
						return row.State == runtimetypes.HITLApprovalApproved, nil
					}
				} else {
					return false, fmt.Errorf("hitlservice: persist pending approval: %w", err)
				}
			} else {
				return false, fmt.Errorf("hitlservice: persist pending approval: %w", err)
			}
		}
	}

	// Buffered (capacity 1): a Respond landing before this goroutine reaches
	// the select below is recorded on the channel instead of discarded by a
	// `default:` arm — that unconditional discard was defect D2 (the old
	// unbuffered send-with-default Respond had zero callers repo-wide and
	// would have dropped answers if wired).
	ch := make(chan answer, 1)
	s.mu.Lock()
	s.pending[approvalID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, approvalID)
		s.mu.Unlock()
	}()

	ev := taskengine.NewTaskEvent(ctx, taskengine.TaskEventApprovalRequested)
	ev.ApprovalID = approvalID
	ev.HookName = req.ToolsName
	ev.ToolName = req.ToolName
	ev.ApprovalArgs = req.Args
	ev.ApprovalDiff = req.Diff
	if err := sink.PublishTaskEvent(ctx, ev); err != nil {
		return false, fmt.Errorf("hitl: publish approval request: %w", err)
	}

	waitCtx := ctx
	if !ruleTimeout {
		// Nothing upstream bounds ctx in this case (localtools.HITLWrapper
		// only wraps its askCtx with a deadline when TimeoutS > 0) — this is
		// exactly the unbounded-hang path (fleet-consolidation.md D1).  Apply
		// the serve-level ceiling ourselves.
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeoutDur)
		defer cancel()
	}

	// The channel wakes a same-process Respond immediately; the poll watches
	// the DURABLE row for a resolver in another process (the child unit's own
	// inline resolve on an adopted row, or `contenox approvals respond` in a
	// second terminal) — the same two-lane wait RequestAttention runs.
	poll := time.NewTicker(attentionPollInterval)
	defer poll.Stop()
	for {
		select {
		case ans := <-ch:
			return ans.approved, nil
		case <-poll.C:
			row, err := s.approvals.GetHITLApproval(ctx, approvalID)
			if err != nil || row.State == runtimetypes.HITLApprovalPending {
				continue // unreadable right now, or still waiting on a human
			}
			return row.State == runtimetypes.HITLApprovalApproved, nil
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				// The caller's own context ended: process shutdown, client
				// disconnect, or — when the matched rule set TimeoutS>0 — the
				// exact deadline localtools.HITLWrapper.Exec applied to askCtx,
				// which it detects via this same ctx.Err() to apply the rule's
				// OnTimeout itself (unchanged from before this change). The row
				// is left pending here either way: SweepExpired closes it out
				// once expires_at passes, and a restart before that still finds
				// it pending rather than losing it.
				return false, ctx.Err()
			}
			// Only the serve-level ceiling could have fired here (a rule timeout
			// would have made waitCtx == ctx, handled above): nothing upstream
			// bounds this ask, so treat it exactly like an explicit human denial
			// — a late denial beats an eternal block — instead of hanging
			// forever. SweepExpired resolves the row itself on its next tick.
			return false, nil
		}
	}
}

// buildApprovalRow assembles the durable pending row for one ask. Spelled
// once so RequestApproval (the blocking fleet path) and RecordPendingApproval
// (the HITL wrapper's park-and-maybe-suspend path) can never drift in what an
// inbox row carries.
func buildApprovalRow(approvalID string, req ApprovalRequest, now time.Time, timeoutDur time.Duration) *runtimetypes.HITLApproval {
	onTimeout := req.OnTimeout
	if onTimeout == "" {
		onTimeout = ActionDeny
	}
	row := &runtimetypes.HITLApproval{
		ID:          approvalID,
		ToolsName:   req.ToolsName,
		ToolName:    req.ToolName,
		ArgsSummary: summarizeApprovalArgs(req.Args),
		PolicyName:  req.PolicyName,
		MatchedRule: req.MatchedRule,
		OnTimeout:   string(onTimeout),
		State:       runtimetypes.HITLApprovalPending,
		InstanceID:  req.InstanceID,
		SessionID:   req.SessionID,
		AgentName:   req.AgentName,
		CreatedAt:   now,
		ExpiresAt:   now.Add(timeoutDur),
	}
	if req.MissionID != "" {
		// Nullable by design: an ask with no mission stores NULL rather than an
		// empty string, so "not on a mission" and "mission unknown" cannot be
		// confused (see runtimetypes.HITLApproval).
		missionID := req.MissionID
		row.MissionID = &missionID
	}
	if req.Diff != "" {
		diff := req.Diff
		row.Diff = &diff
	}
	return row
}

// RecordPendingApproval implements ApprovalRecorder: it durably records the
// ask under the CALLER-chosen approvalID (the engine-minted tool-call ID, so
// approval == scope.tool_call == checkpoint key) without parking a waiter —
// the wrapper owns the park. Expiry follows the same rule RequestApproval
// applies: the matched rule's TimeoutS when set, the serve-level ceiling
// otherwise, so an ask nobody ever answers is closed out by SweepExpired.
func (s *service) RecordPendingApproval(ctx context.Context, approvalID string, req ApprovalRequest) error {
	if s.approvals == nil {
		return fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	if strings.TrimSpace(approvalID) == "" {
		return fmt.Errorf("hitlservice: RecordPendingApproval requires a non-empty approval ID")
	}
	timeoutDur := s.ceiling()
	if req.TimeoutS > 0 {
		timeoutDur = time.Duration(req.TimeoutS) * time.Second
	}
	row := buildApprovalRow(approvalID, req, time.Now().UTC(), timeoutDur)
	if err := s.approvals.CreateHITLApproval(ctx, row); err != nil {
		return fmt.Errorf("hitlservice: persist pending approval %s: %w", approvalID, err)
	}
	return nil
}

// ResolveApprovalInline implements ApprovalRecorder: the fast-path resolve
// used by the waiter ITSELF (the HITL wrapper closing its own row after an
// in-session verdict or a rule timeout). It never invokes the resume hook —
// there is nothing suspended to resume when the waiter is still present.
func (s *service) ResolveApprovalInline(ctx context.Context, approvalID string, approved bool) error {
	return s.resolve(ctx, approvalID, approved, false)
}

// Respond implements Service.Respond: see its doc on the interface. When the
// verdict lands for an approval whose waiter is GONE (the S6 released case —
// the run suspended to a checkpoint), the registered resume hook runs the
// suspended chain IN THIS PROCESS; see ResumeHook for the error contract.
func (s *service) Respond(ctx context.Context, approvalID string, approved bool) error {
	return s.resolve(ctx, approvalID, approved, true)
}

func (s *service) resolve(ctx context.Context, approvalID string, approved bool, runHook bool) error {
	if s.approvals == nil {
		return fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	state := runtimetypes.HITLApprovalApproved
	if !approved {
		state = runtimetypes.HITLApprovalDenied
	}
	now := time.Now().UTC()
	err := s.approvals.ResolveHITLApproval(ctx, approvalID, state, marshalApprovalResolution(approved), now)
	if err != nil {
		if !errors.Is(err, libdb.ErrNotFound) {
			return fmt.Errorf("hitlservice: resolve approval %s: %w", approvalID, err)
		}
		// The compare-and-swap matched zero rows: id either does not exist or
		// is no longer pending. Re-read the row to tell those apart (and,
		// among terminal states, expired from already-answered) rather than
		// collapsing both into one generic failure.
		row, getErr := s.approvals.GetHITLApproval(ctx, approvalID)
		if getErr != nil {
			if errors.Is(getErr, libdb.ErrNotFound) {
				return ErrApprovalNotFound
			}
			return fmt.Errorf("hitlservice: look up approval %s: %w", approvalID, getErr)
		}
		if row.State == runtimetypes.HITLApprovalExpired {
			return ErrApprovalExpired
		}
		return ErrApprovalAlreadyResolved
	}

	// Best-effort in-process wake-up: a requester parked on THIS instance
	// gets the answer immediately over the buffered channel. When none is
	// parked (a different hitlservice instance/process — e.g. after a
	// restart — or the requester already returned via its own timeout), the
	// row transition above is the durable record of the answer; that is
	// precisely the drop this buffered channel + persisted row combination
	// fixes (defect D2).
	s.mu.Lock()
	ch, ok := s.pending[approvalID]
	hook := s.resumeHook
	s.mu.Unlock()
	if ok {
		select {
		case ch <- answer{approved: approved}:
		default:
			// Should not happen (the CAS above guarantees at most one winning
			// Respond per row, and the channel is fresh per RequestApproval
			// call), but never block Respond on a full channel.
		}
		return nil
	}

	// Waiter GONE (S6 released case): the run that raised this ask suspended
	// to a checkpoint and its goroutine ended. The responding process runs the
	// resume itself, synchronously — `approvals respond` completing means the
	// chain ran, exactly like answering in-session means the chain continued.
	// ErrNoCheckpoint is the clean "nothing was suspended" answer (fast-path
	// rows resolve via ResolveApprovalInline and never reach here, but a
	// RequestApproval waiter that timed out leaves the same shape).
	if runHook && hook != nil {
		if err := hook(ctx, approvalID); err != nil && !errors.Is(err, ErrNoCheckpoint) {
			// The verdict IS durably recorded — never un-answered — but the run
			// did not resume; its checkpoint is retained with a failure
			// annotation so it is not silently lost.
			return fmt.Errorf("hitlservice: verdict for approval %s recorded, but resuming its suspended run failed: %w", approvalID, err)
		}
	}
	return nil
}

// sweepBatchLimit caps how many expired rows one SweepExpired call resolves,
// so a large backlog cannot make a single call block indefinitely; the next
// tick picks up whatever is left.
const sweepBatchLimit = 200

// SweepExpired implements Service.SweepExpired: see its doc on the interface.
func (s *service) SweepExpired(ctx context.Context) (int, error) {
	if s.approvals == nil {
		return 0, nil
	}
	now := time.Now().UTC()
	rows, err := s.approvals.ListExpiredHITLApprovals(ctx, now, sweepBatchLimit)
	if err != nil {
		return 0, fmt.Errorf("hitlservice: list expired approvals: %w", err)
	}
	expired := 0
	for _, row := range rows {
		approved := onTimeoutOutcome(Action(row.OnTimeout))
		err := s.approvals.ResolveHITLApproval(ctx, row.ID, runtimetypes.HITLApprovalExpired, marshalApprovalResolution(approved), now)
		if err != nil {
			if errors.Is(err, libdb.ErrNotFound) {
				continue // already resolved by a racing Respond; nothing to do
			}
			return expired, fmt.Errorf("hitlservice: resolve expired approval %s: %w", row.ID, err)
		}
		expired++
		// Best-effort wake-up, in case a requester is somehow still parked
		// on this id in this process (should not normally happen once its
		// own deadline has passed, but costs nothing to cover).
		s.mu.Lock()
		ch, ok := s.pending[row.ID]
		hook := s.resumeHook
		s.mu.Unlock()
		if ok {
			select {
			case ch <- answer{approved: approved}:
			default:
			}
			continue
		}
		// S6: an expired approval may back a SUSPENDED run. Resume it with the
		// timeout outcome (deny unless the rule said allow, which a validated
		// policy cannot) so the chain finishes with the existing deny
		// semantics instead of its checkpoint lingering forever. Best-effort:
		// a resume failure is reported, keeps the checkpoint annotated (the
		// hook's contract), and never fails the sweep.
		if hook != nil {
			if err := s.resumeExpired(ctx, hook, row.ID); err != nil {
				reportErr, _, end := s.tracker.Start(ctx, "hitl", "sweep_resume", "approval_id", row.ID)
				reportErr(err)
				end()
			}
		}
	}
	return expired, nil
}

// resumeExpired runs the resume hook for a swept row, treating "nothing was
// suspended" as the clean no-op it is.
func (s *service) resumeExpired(ctx context.Context, hook ResumeHook, approvalID string) error {
	if err := hook(ctx, approvalID); err != nil && !errors.Is(err, ErrNoCheckpoint) {
		return err
	}
	return nil
}

// ListPending implements Service.ListPending: see its doc on the interface.
// Unlike SweepExpired (a best-effort background tick that silently no-ops
// with nothing configured to sweep), ListPending is a direct, user-facing
// read — like RequestApproval/Respond, it reports the missing durable store
// as an explicit error rather than a quietly empty inbox that could be
// mistaken for "nothing pending".
func (s *service) ListPending(ctx context.Context, limit int) ([]*runtimetypes.HITLApproval, error) {
	if s.approvals == nil {
		return nil, fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	rows, err := s.approvals.ListHITLApprovals(ctx, runtimetypes.HITLApprovalPending, nil, limit)
	if err != nil {
		return nil, fmt.Errorf("hitlservice: list pending approvals: %w", err)
	}
	if rows == nil {
		rows = []*runtimetypes.HITLApproval{}
	}
	return rows, nil
}

// onTimeoutOutcome mirrors localtools.HITLWrapper.Exec's own on-timeout
// branch (runtime/localtools/hitl.go, the block right after the approval
// select) exactly: only an explicit ActionAllow resolves to an
// auto-approval; every other value denies. In practice a *validated* policy
// can never set on_timeout to "allow" (policy.go's validatePolicy rejects it
// outright, since it would silently bypass approval), so this only ever
// observably returns false — kept as a real branch rather than hardcoded so
// it stays byte-for-byte consistent with what localtools already decided for
// the tool call itself if that constraint ever changes.
func onTimeoutOutcome(onTimeout Action) bool {
	return onTimeout == ActionAllow
}

// approvalResolution is the structured payload written into
// HITLApproval.Resolution. Today the only shape ever written is a boolean
// approve/deny answer — Resolution is not narrowed to a bare boolean column
// because a permission ask is answered yes/no, but a later ask kind answers
// with data instead ("which of these three?", "what value should I use?");
// keeping the stored payload structured now means that shape needs no
// migration later. Respond's own signature stays boolean-only in this slice
// (Approved is simply the only field written); only the storage
// representation is forward-looking.
type approvalResolution struct {
	Approved *bool `json:"approved,omitempty"`
	// Answer carries an ATTENTION ask's reply — the operator's own words, which
	// is the whole difference between the two ask kinds. A permission ask is
	// answered yes/no; an attention ask ("which project did you mean?") is
	// answered with data, and that data is what the asking unit receives back as
	// its tool result. The forward-looking shape this column was given (see
	// runtimetypes.HITLApproval.Resolution) is what let this land without a
	// migration.
	Answer *string `json:"answer,omitempty"`
	// AnsweredBy records WHO answered an attention ask when it was not a human —
	// today only "agent" (a supervising session's model answering its own
	// subagent). Absent means a human answered, which is the norm and needs no
	// marker. See AnswerAsAgent.
	AnsweredBy *string `json:"answeredBy,omitempty"`
}

// answer is what a parked requester is woken with. RequestApproval reads only
// Approved; RequestAttention reads Text. One channel type serves both because
// both kinds resolve through the same durable row and the same Respond/Answer
// compare-and-swap — a second waiter mechanism for the second ask kind would be
// the "no second mechanism" invariant broken for no gain.
type answer struct {
	approved bool
	text     string
}

// marshalApprovalResolution encodes approved as the current resolution
// payload shape. Marshaling a fixed, trivially-encodable struct cannot fail
// in practice; the fallback exists only so a caller never has to handle an
// error from what is conceptually an infallible encode.
// marshalAttentionResolution encodes an attention ask's answer text as the
// stored resolution payload.
func marshalAttentionResolution(text, by string) json.RawMessage {
	res := approvalResolution{Answer: &text}
	if by != "" {
		res.AnsweredBy = &by
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// AbandonMissionAsks implements Service — see the interface doc. The CAS in
// ResolveHITLApproval keeps it race-safe against a concurrent Respond/Answer:
// whichever transition lands first wins, and a row that just resolved is
// simply skipped here.
func (s *service) AbandonMissionAsks(ctx context.Context, missionID string) ([]string, error) {
	if s.approvals == nil {
		return nil, fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	rows, err := s.approvals.ListHITLApprovalsForMission(ctx, missionID, runtimetypes.MAXLIMIT)
	if err != nil {
		return nil, fmt.Errorf("hitlservice: list asks for mission %s: %w", missionID, err)
	}
	now := time.Now().UTC()
	var closed []string
	for _, row := range rows {
		if row.State != runtimetypes.HITLApprovalPending {
			continue
		}
		err := s.approvals.ResolveHITLApproval(ctx, row.ID, runtimetypes.HITLApprovalDenied, marshalApprovalResolution(false), now)
		if err != nil {
			if errors.Is(err, libdb.ErrNotFound) {
				continue // resolved concurrently — not ours to close anymore
			}
			return closed, fmt.Errorf("hitlservice: abandon ask %s: %w", row.ID, err)
		}
		// Wake a waiter parked on THIS instance so it sees the deny now rather
		// than at its own timeout. Same best-effort shape as resolve().
		s.mu.Lock()
		ch, ok := s.pending[row.ID]
		s.mu.Unlock()
		if ok {
			select {
			case ch <- answer{approved: false}:
			default:
			}
		}
		closed = append(closed, row.ID)
	}
	return closed, nil
}

func marshalApprovalResolution(approved bool) json.RawMessage {
	raw, err := json.Marshal(approvalResolution{Approved: &approved})
	if err != nil {
		return json.RawMessage(fmt.Sprintf(`{"approved":%t}`, approved))
	}
	return raw
}

// summarizeApprovalArgs picks a human-recognizable field out of a tool call's
// args for the durable row's ArgsSummary column, mirroring localtools'
// hitlArgsSummary heuristic. Duplicated (not imported) because localtools
// already imports hitlservice — importing back would cycle.
func summarizeApprovalArgs(args map[string]any) string {
	for _, key := range []string{"path", "command", "url", "pattern"} {
		v, ok := args[key].(string)
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		s := strings.TrimSpace(strings.ReplaceAll(v, "\n", " "))
		if len([]rune(s)) > 96 {
			return string([]rune(s)[:95]) + "..."
		}
		return s
	}
	return ""
}
