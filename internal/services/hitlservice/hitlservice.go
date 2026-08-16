// Package hitlservice evaluates approval policies for tool calls, returning
// allow/deny/approve decisions. The caller pauses execution and sources the
// human decision.
package hitlservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"

	libdb "github.com/contenox/contenox/libdbexec"
)

func (s *service) hitlLog(ctx context.Context, msg string, kv ...any) {
	_, _, end := s.tracker.Start(ctx, "hitl", msg, kv...)
	end()
}

type KVReader interface {
	GetKV(ctx context.Context, key string, out interface{}) error
}

type PolicyEvaluator interface {
	Evaluate(ctx context.Context, toolsName, toolName string, args map[string]any) (EvaluationResult, error)
}

// ComputeBoundsReader loads a policy's compute ceiling. Optional, reached via
// type assertion.
type ComputeBoundsReader interface {
	ComputeBoundsFor(ctx context.Context, policyName string) (ComputeBounds, error)
}

// PolicyValidator reports whether a named HITL policy exists and loads.
type PolicyValidator interface {
	ValidatePolicy(ctx context.Context, policyName string) error
}

// NewPolicyValidator returns a PolicyValidator over src, using Evaluate's
// fallback chain for an empty policy name.
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

type approvalStore interface {
	CreateHITLApproval(ctx context.Context, a *runtimetypes.HITLApproval) error
	GetHITLApproval(ctx context.Context, id string) (*runtimetypes.HITLApproval, error)
	ResolveHITLApproval(ctx context.Context, id string, state runtimetypes.HITLApprovalState, resolution json.RawMessage, resolvedAt time.Time) error
	ResolveHITLApprovalWithinBound(ctx context.Context, id string, bound runtimetypes.AgentAnswerBound, state runtimetypes.HITLApprovalState, resolution json.RawMessage, resolvedAt time.Time) error
	ListExpiredHITLApprovals(ctx context.Context, asOf time.Time, limit int) ([]*runtimetypes.HITLApproval, error)
	ListHITLApprovals(ctx context.Context, state runtimetypes.HITLApprovalState, createdAtCursor *time.Time, limit int) ([]*runtimetypes.HITLApproval, error)
	ListHITLApprovalsForMission(ctx context.Context, missionID string, limit int) ([]*runtimetypes.HITLApproval, error)
	ListPendingHITLApprovalsForSession(ctx context.Context, sessionID string, limit int) ([]*runtimetypes.HITLApproval, error)
}

type checkpointReader interface {
	GetChainCheckpoint(ctx context.Context, id string) (*runtimetypes.ChainCheckpoint, error)
}

type Service interface {
	PolicyEvaluator

	// RequestApproval durably records req, publishes approval_requested, then
	// blocks until Respond answers or the wait is bounded.
	RequestApproval(ctx context.Context, req ApprovalRequest, sink taskengine.TaskEventSink) (bool, error)

	// Respond transitions a pending approval to approved/denied, waking any
	// requester parked on it.
	Respond(ctx context.Context, approvalID string, approved bool) error

	// RequestAttention durably records a unit's question and blocks until
	// Answer replies, the ceiling expires it, or ctx ends.
	RequestAttention(ctx context.Context, req AttentionRequest, sink taskengine.TaskEventSink) (string, error)

	// Answer resolves a pending attention ask with the operator's reply and
	// wakes the unit parked on it; rejects a permission ask.
	Answer(ctx context.Context, askID, text string) error

	// AnswerAsAgent is Answer, recorded as answered by a supervising agent
	// rather than a human.
	AnswerAsAgent(ctx context.Context, askID, text string) error

	// AnswerAsAgentNamed is AnswerAsAgent with the answering agent's name as
	// the recorded actor.
	AnswerAsAgentNamed(ctx context.Context, askID, agentName, text string) error

	// AnswerAsAgentBounded is AnswerAsAgentNamed under an atomic per-mission cap
	// of max agent-answered asks, else ErrAgentAnswerBoundSpent.
	AnswerAsAgentBounded(ctx context.Context, askID, agentName, text string, max int) error

	// PendingAttentionAsks returns a mission's unanswered questions, newest first.
	PendingAttentionAsks(ctx context.Context, missionID string) ([]*runtimetypes.HITLApproval, error)

	// AttentionBoundsFor reports whether and how often the supervising agent
	// may answer a unit's question.
	AttentionBoundsFor(ctx context.Context, policyName string) (AttentionBounds, error)

	// AgentAnswerCount reports how many of a mission's questions an agent answered.
	AgentAnswerCount(ctx context.Context, missionID string) (int, error)

	// RespondAsAgentBounded records a permission verdict attributed to an agent
	// under an atomic per-mission cap, with optional guidance a denial carries.
	RespondAsAgentBounded(ctx context.Context, askID, agentName string, approved bool, guidance string, max int) error

	// AgentApprovalCount reports how many of a mission's gated tool calls an agent decided.
	AgentApprovalCount(ctx context.Context, missionID string) (int, error)

	// AgentGuidanceFor returns the redirects an adjudicating agent attached to
	// the calls it refused on a mission, oldest first.
	AgentGuidanceFor(ctx context.Context, missionID string) ([]GuidanceNote, error)

	// SweepExpired resolves every pending approval past its deadline,
	// applying OnTimeout, and returns the count resolved.
	SweepExpired(ctx context.Context) (int, error)

	// ListPending returns pending approvals newest first, bounded by limit.
	ListPending(ctx context.Context, limit int) ([]*runtimetypes.HITLApproval, error)

	// ListPendingForSession is ListPending narrowed to one session's asks.
	ListPendingForSession(ctx context.Context, sessionID string, limit int) ([]*runtimetypes.HITLApproval, error)

	// AbandonMissionAsks resolves every pending ask under missionID denied
	// without running the resume hook, and returns the closed IDs.
	AbandonMissionAsks(ctx context.Context, missionID string) ([]string, error)
}

var (
	// ErrApprovalNotFound reports that approvalID does not exist in the store.
	ErrApprovalNotFound = errors.New("hitlservice: approval not found")
	// ErrApprovalAlreadyResolved reports approvalID was already answered.
	ErrApprovalAlreadyResolved = errors.New("hitlservice: approval already answered")
	// ErrApprovalExpired reports the sweeper resolved approvalID before this Respond landed.
	ErrApprovalExpired = errors.New("hitlservice: approval expired before it was answered")
	// ErrNoCheckpoint reports no run is checkpointed under this approval; Respond treats it as a no-op.
	ErrNoCheckpoint = errors.New("hitlservice: no suspended run is checkpointed under this approval")
	// ErrAgentAnswerBoundSpent reports the mission's agent-answer cap was already spent.
	ErrAgentAnswerBoundSpent = errors.New("hitlservice: mission agent-answer bound spent")
	// ErrVerdictNeedsResumer reports a verdict refused before recording because
	// this process can neither park a waiter nor resume the checkpointed run.
	ErrVerdictNeedsResumer = errors.New("hitlservice: a suspended run is checkpointed under this ask and this process cannot resume it; the verdict was not recorded and the ask is still pending")
)

const approvalPollInterval = time.Second

// ResumeHook resumes the suspended run checkpointed under approvalID once its
// verdict lands with nobody parked. Return ErrNoCheckpoint for a clean no-op.
type ResumeHook func(ctx context.Context, approvalID string) error

// ApprovalRecorder creates the pending row before parking, and resolves it
// inline on a fast-path verdict without triggering the resume hook.
type ApprovalRecorder interface {
	RecordPendingApproval(ctx context.Context, approvalID string, req ApprovalRequest) error
	ResolveApprovalInline(ctx context.Context, approvalID string, approved bool) error
}

const defaultPolicyName = "hitl-policy-default.json"

// DefaultApprovalCeiling bounds RequestApproval when the matched rule sets no
// TimeoutS of its own.
const DefaultApprovalCeiling = time.Hour

type service struct {
	src            PolicySource
	tenantID       string
	store          KVReader
	workspaceID    string
	tracker        libtracker.ActivityTracker
	fallbackPolicy string
	approvals      approvalStore
	checkpoints    checkpointReader

	mu              sync.Mutex
	pending         map[string]chan answer
	offered         map[string]struct{}
	approvalCeiling time.Duration
	resumeHook      ResumeHook
	adjudicator     Adjudicator
}

// New constructs a hitlservice bound to a tenant.
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
		offered:        make(map[string]struct{}),
	}
	if as, ok := store.(approvalStore); ok {
		svc.approvals = as
	}
	if cr, ok := store.(checkpointReader); ok {
		svc.checkpoints = cr
	}
	return svc
}

func (s *service) requireResumerForVerdict(ctx context.Context, askID string) error {
	if s.checkpoints == nil {
		return nil
	}
	s.mu.Lock()
	_, hasWaiter := s.pending[askID]
	hook := s.resumeHook
	s.mu.Unlock()
	if hasWaiter || hook != nil {
		return nil
	}
	if _, err := s.checkpoints.GetChainCheckpoint(ctx, askID); err != nil {
		return nil
	}
	return fmt.Errorf("%w (ask %s)", ErrVerdictNeedsResumer, askID)
}

// SetApprovalCeiling overrides the approval-wait ceiling on svc.
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

// SetWorkspaceID binds svc to the workspace whose active-policy row Evaluate
// reads. An unset workspace reads the global row.
func SetWorkspaceID(svc Service, workspaceID string) {
	if s, ok := svc.(*service); ok {
		s.mu.Lock()
		s.workspaceID = strings.TrimSpace(workspaceID)
		s.mu.Unlock()
	}
}

func (s *service) workspace() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workspaceID
}

// SetResumeHook registers the resume-on-verdict hook on svc.
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

type policyNameContextKey struct{}

// WithPolicyName returns a context pinning HITL evaluation to policyName.
func WithPolicyName(ctx context.Context, policyName string) context.Context {
	policyName = strings.TrimSpace(policyName)
	if policyName == "" {
		return ctx
	}
	return context.WithValue(ctx, policyNameContextKey{}, policyName)
}

// PolicyNameFromContext returns the policy override WithPolicyName set, if any.
func PolicyNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(policyNameContextKey{}).(string)
	return strings.TrimSpace(name)
}

func (s *service) readActivePolicyName(ctx context.Context) string {
	if r, ok := s.store.(clikv.Reader); ok {
		return clikv.ReadHITLPolicy(ctx, r, s.workspace())
	}
	return clikv.Read(ctx, s.store, clikv.KeyHITLPolicyName)
}

func (s *service) Evaluate(ctx context.Context, toolsName, toolName string, args map[string]any) (EvaluationResult, error) {
	reportErr, reportChange, end := s.tracker.Start(ctx, "hitl", "evaluate", "toolsName", toolsName, "toolName", toolName)
	defer end()
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
	result := evaluate(ctx, p, toolsName, toolName, args)
	result.PolicyName = policyPath
	if result.Detail != "" {
		reportChange("detail", result.Detail)
	}
	return result, nil
}

func (s *service) RequestApproval(ctx context.Context, req ApprovalRequest, sink taskengine.TaskEventSink) (bool, error) {
	if s.approvals == nil {
		return false, fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	// A caller-supplied ToolCallID is the ask's durable identity; a repeat call adopts the existing row.
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

	ruleTimeout := req.TimeoutS > 0
	timeoutDur := s.ceiling()
	if ruleTimeout {
		timeoutDur = time.Duration(req.TimeoutS) * time.Second
	}

	if !adopted {
		row := buildApprovalRow(approvalID, req, time.Now().UTC(), timeoutDur)
		if err := s.approvals.CreateHITLApproval(ctx, row); err != nil {
			// A create race on a shared ToolCallID: the loser re-reads and adopts.
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

	// Buffered so a Respond landing early is recorded, not dropped.
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

	// Offered after the waiter is registered, so a verdict that lands immediately
	// wakes it rather than racing it.
	s.offer(ctx, adjudicationFromApprovalRequest(approvalID, req))

	waitCtx := ctx
	if !ruleTimeout {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeoutDur)
		defer cancel()
	}

	// The channel wakes a same-process Respond; the poll watches for a resolver elsewhere.
	poll := time.NewTicker(approvalPollInterval)
	defer poll.Stop()
	for {
		select {
		case ans := <-ch:
			return ans.approved, nil
		case <-poll.C:
			row, err := s.approvals.GetHITLApproval(ctx, approvalID)
			if err != nil || row.State == runtimetypes.HITLApprovalPending {
				continue
			}
			return row.State == runtimetypes.HITLApprovalApproved, nil
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			// The serve ceiling fired; treated as denial, SweepExpired resolves the row later.
			return false, nil
		}
	}
}

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
		missionID := req.MissionID
		row.MissionID = &missionID
	}
	if req.Diff != "" {
		diff := req.Diff
		row.Diff = &diff
	}
	return row
}

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
	// The in-process gating path records its row here rather than through
	// RequestApproval, so this is where its asks reach an adjudicator.
	s.offer(ctx, adjudicationFromApprovalRequest(approvalID, req))
	return nil
}

func (s *service) ResolveApprovalInline(ctx context.Context, approvalID string, approved bool) error {
	return s.resolve(ctx, approvalID, approved, false)
}

func (s *service) Respond(ctx context.Context, approvalID string, approved bool) error {
	return s.resolve(ctx, approvalID, approved, true)
}

func (s *service) resolve(ctx context.Context, approvalID string, approved bool, runHook bool) error {
	if s.approvals == nil {
		return fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	// The capability check precedes the CAS so an unresumable process never spends the one-shot verdict.
	if runHook {
		if err := s.requireResumerForVerdict(ctx, approvalID); err != nil {
			return err
		}
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
		// Zero rows matched: id doesn't exist, or isn't pending; re-read to tell which.
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
	s.hitlLog(ctx, "verdict recorded", "approval_id", approvalID, "approved", approved)
	s.forgetOffer(approvalID)

	s.mu.Lock()
	ch, ok := s.pending[approvalID]
	hook := s.resumeHook
	s.mu.Unlock()
	if ok {
		s.hitlLog(ctx, "waiter woken", "approval_id", approvalID, "approved", approved)
		select {
		case ch <- answer{approved: approved}:
		default:
		}
		return nil
	}

	// Waiter gone: resume the suspended run synchronously.
	if runHook && hook != nil {
		if err := hook(ctx, approvalID); err != nil && !errors.Is(err, ErrNoCheckpoint) {
			s.hitlLog(ctx, "turn failed", "approval_id", approvalID, "reason", "resume_failed", "error", err.Error())
			return fmt.Errorf("hitlservice: verdict for approval %s recorded, but resuming its suspended run failed: %w", approvalID, err)
		}
		s.hitlLog(ctx, "tool resumed", "approval_id", approvalID, "approved", approved)
	}
	return nil
}

const sweepBatchLimit = 200

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
				continue // already resolved by a racing Respond
			}
			return expired, fmt.Errorf("hitlservice: resolve expired approval %s: %w", row.ID, err)
		}
		expired++
		s.forgetOffer(row.ID)
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
		// An expired approval may back a suspended run; a failed resume never fails the sweep.
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

func (s *service) resumeExpired(ctx context.Context, hook ResumeHook, approvalID string) error {
	if err := hook(ctx, approvalID); err != nil && !errors.Is(err, ErrNoCheckpoint) {
		return err
	}
	return nil
}

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

func (s *service) ListPendingForSession(ctx context.Context, sessionID string, limit int) ([]*runtimetypes.HITLApproval, error) {
	if s.approvals == nil {
		return nil, fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	rows, err := s.approvals.ListPendingHITLApprovalsForSession(ctx, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("hitlservice: list pending approvals for session %q: %w", sessionID, err)
	}
	if rows == nil {
		rows = []*runtimetypes.HITLApproval{}
	}
	return rows, nil
}

func onTimeoutOutcome(onTimeout Action) bool {
	return onTimeout == ActionAllow
}

type approvalResolution struct {
	Approved   *bool   `json:"approved,omitempty"`
	Answer     *string `json:"answer,omitempty"`
	AnsweredBy *string `json:"answeredBy,omitempty"`
	DecidedBy  *string `json:"decidedBy,omitempty"`
	Guidance   *string `json:"guidance,omitempty"`
}

type answer struct {
	approved bool
	text     string
}

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
				continue // resolved concurrently
			}
			return closed, fmt.Errorf("hitlservice: abandon ask %s: %w", row.ID, err)
		}
		s.mu.Lock()
		ch, ok := s.pending[row.ID]
		s.mu.Unlock()
		if ok {
			select {
			case ch <- answer{approved: false}:
			default:
			}
		}
		s.forgetOffer(row.ID)
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
