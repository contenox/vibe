// Package hitlservice evaluates approval policies for tool calls, returning
// allow/deny/approve decisions. The caller (typically localtools.HITLWrapper)
// is responsible for pausing execution and sourcing the human decision.
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

// hitlLog reports one approval-lifecycle step through the service's tracker,
// so fields are redacted and the sink is composition-chosen.
// Warn, so an Info-level line is silently dropped there).
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

// ComputeBoundsReader loads a policy's compute ceiling (see ComputeBounds).
// Optional: reached via type assertion; a Service without it is unbounded.
type ComputeBoundsReader interface {
	ComputeBoundsFor(ctx context.Context, policyName string) (ComputeBounds, error)
}

// PolicyValidator reports whether a named HITL policy exists and loads. It
// validates strictly at creation time (unlike ComputeBoundsFor's runtime
// fail-open), so a typo cannot silently substitute the default gate.
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

// approvalStore is the durable persistence surface RequestApproval, Respond,
// SweepExpired, and ListPending need — a subset of runtimetypes.Store so
// hitlservice depends only on the methods it calls. service.store is
// type-asserted against it at construction; callers passing a bare KVReader
// never exercise these methods — see the nil checks at the top of each.
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

	// RequestApproval durably records req, publishes approval_requested, then
	// blocks until Respond answers or the wait is bounded (a matched rule's
	// TimeoutS, else DefaultApprovalCeiling).
	RequestApproval(ctx context.Context, req ApprovalRequest, sink taskengine.TaskEventSink) (bool, error)

	// Respond transitions a pending approval to approved/denied and wakes any
	// requester parked on it. Returns ErrApprovalNotFound,
	// ErrApprovalAlreadyResolved, or ErrApprovalExpired instead of a bare false.
	Respond(ctx context.Context, approvalID string, approved bool) error

	// RequestAttention durably records a unit's question and blocks until
	// Answer replies, the ceiling expires it, or ctx ends. See attention.go.
	RequestAttention(ctx context.Context, req AttentionRequest, sink taskengine.TaskEventSink) (string, error)

	// Answer resolves a pending attention ask with the operator's reply and
	// wakes the unit parked on it; rejects a permission ask.
	Answer(ctx context.Context, askID, text string) error

	// AnswerAsAgent is Answer, recorded as answered by a supervising agent
	// rather than a human.
	AnswerAsAgent(ctx context.Context, askID, text string) error

	// PendingAttentionAsks returns a mission's unanswered questions, newest first.
	PendingAttentionAsks(ctx context.Context, missionID string) ([]*runtimetypes.HITLApproval, error)

	// AttentionBoundsFor reports whether and how often the supervising agent
	// may answer a unit's question; the zero (human-only) bounds are the default.
	AttentionBoundsFor(ctx context.Context, policyName string) (AttentionBounds, error)

	// AgentAnswerCount reports how many of a mission's questions an agent answered.
	AgentAnswerCount(ctx context.Context, missionID string) (int, error)

	// SweepExpired resolves every pending approval past its deadline,
	// applying OnTimeout. Serve runs it on an interval; returns the count resolved.
	SweepExpired(ctx context.Context) (int, error)

	// ListPending returns pending approvals newest first, bounded by limit,
	// always a non-nil slice.
	ListPending(ctx context.Context, limit int) ([]*runtimetypes.HITLApproval, error)

	// AbandonMissionAsks closes every pending ask under missionID without
	// running the resume hook; each row resolves denied. Returns the closed IDs.
	AbandonMissionAsks(ctx context.Context, missionID string) ([]string, error)
}

// Sentinel errors Respond returns instead of a silent false.
var (
	// ErrApprovalNotFound reports that approvalID does not exist in the store.
	ErrApprovalNotFound = errors.New("hitlservice: approval not found")
	// ErrApprovalAlreadyResolved reports approvalID was already answered.
	ErrApprovalAlreadyResolved = errors.New("hitlservice: approval already answered")
	// ErrApprovalExpired reports the sweeper resolved approvalID before this Respond landed.
	ErrApprovalExpired = errors.New("hitlservice: approval expired before it was answered")
	// ErrNoCheckpoint reports no run is checkpointed under this approval; Respond treats it as a no-op.
	ErrNoCheckpoint = errors.New("hitlservice: no suspended run is checkpointed under this approval")
)

// ResumeHook resumes the suspended run checkpointed under approvalID once
// its verdict lands with nobody parked on it. The composition root registers
// it via SetResumeHook. Return ErrNoCheckpoint for a clean no-op; any other
// error is surfaced by Respond with the verdict still recorded.
type ResumeHook func(ctx context.Context, approvalID string) error

// ApprovalRecorder is the durable half of the HITL wrapper's suspend path:
// create the pending row before parking, and resolve it inline on a
// fast-path verdict without triggering the resume hook.
type ApprovalRecorder interface {
	RecordPendingApproval(ctx context.Context, approvalID string, req ApprovalRequest) error
	ResolveApprovalInline(ctx context.Context, approvalID string, approved bool) error
}

const defaultPolicyName = "hitl-policy-default.json"

// DefaultApprovalCeiling bounds RequestApproval when the matched rule sets
// no TimeoutS of its own. Override per-service via SetApprovalCeiling.
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
	}
	if as, ok := store.(approvalStore); ok {
		svc.approvals = as
	}
	return svc
}

// SetApprovalCeiling overrides the approval-wait ceiling on svc (when
// constructed by New/NewWithDefaultPolicy). No-op otherwise or for ceiling <= 0.
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

// SetResumeHook registers the resume-on-verdict hook (see ResumeHook) on svc
// (when constructed by New/NewWithDefaultPolicy). No-op for other
// implementations; instance-scoped on purpose, no global registry.
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

// ComputeBoundsFor implements ComputeBoundsReader, resolving policyName
// through the same fallback chain Evaluate uses. A policy that fails to load
// returns the zero (unbounded) bounds and the load error, mirroring
// Evaluate's own fallback to the default policy.
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

// policyNameContextKey scopes a per-request HITL policy override onto a
// context, so concurrent ACP sessions sharing one hitlservice can each pin
// their own policy. Evaluate prefers it over the process-global KV.
type policyNameContextKey struct{}

// WithPolicyName returns a context pinning HITL evaluation to policyName.
// An empty policyName returns ctx unchanged.
func WithPolicyName(ctx context.Context, policyName string) context.Context {
	policyName = strings.TrimSpace(policyName)
	if policyName == "" {
		return ctx
	}
	return context.WithValue(ctx, policyNameContextKey{}, policyName)
}

// PolicyNameFromContext returns the policy override WithPolicyName set, or
// "" when none was.
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
	// A per-request context override wins over the process-global
	// active-policy KV; absent one, fall through to the fallback chain.
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
		// Which command of a compound line was caught.
		reportChange("detail", result.Detail)
	}
	return result, nil
}

func (s *service) RequestApproval(ctx context.Context, req ApprovalRequest, sink taskengine.TaskEventSink) (bool, error) {
	if s.approvals == nil {
		return false, fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	// A caller-supplied ToolCallID is the ask's durable identity; a repeat
	// call with the same ID adopts the existing row rather than minting a twin.
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

	// A matched rule's TimeoutS wins; absent one, the serve ceiling bounds the wait.
	ruleTimeout := req.TimeoutS > 0
	timeoutDur := s.ceiling()
	if ruleTimeout {
		timeoutDur = time.Duration(req.TimeoutS) * time.Second
	}

	if !adopted {
		row := buildApprovalRow(approvalID, req, time.Now().UTC(), timeoutDur)
		// Durable row created before parking, so a restart still shows it pending.
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

	// Buffered (capacity 1) so a Respond landing early is recorded, not dropped.
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
		// Nothing upstream bounds ctx here; apply the serve-level ceiling ourselves.
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeoutDur)
		defer cancel()
	}

	// The channel wakes a same-process Respond; the poll watches the durable
	// row for a resolver in another process.
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
				// The caller's context ended (shutdown, disconnect, or a
				// rule's own deadline); the row is left pending for
				// SweepExpired to close.
				return false, ctx.Err()
			}
			// Only the serve ceiling could fire here; treat it like an
			// explicit denial rather than hanging — SweepExpired resolves
			// the row later.
			return false, nil
		}
	}
}

// buildApprovalRow assembles the durable row shared by RequestApproval and
// RecordPendingApproval so they cannot drift.
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
		// Nullable: no mission stores NULL, distinct from an unknown mission.
		missionID := req.MissionID
		row.MissionID = &missionID
	}
	if req.Diff != "" {
		diff := req.Diff
		row.Diff = &diff
	}
	return row
}

// RecordPendingApproval implements ApprovalRecorder: it records the ask
// under the caller-chosen approvalID without parking a waiter.
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

// ResolveApprovalInline implements ApprovalRecorder's fast-path resolve; it
// never invokes the resume hook since the waiter is still present.
func (s *service) ResolveApprovalInline(ctx context.Context, approvalID string, approved bool) error {
	return s.resolve(ctx, approvalID, approved, false)
}

// Respond implements Service.Respond. When the waiter is gone, the
// registered resume hook runs the suspended chain in this process.
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

	// Best-effort in-process wake-up; when nobody is parked, the row
	// transition above is already the durable record of the answer.
	s.mu.Lock()
	ch, ok := s.pending[approvalID]
	hook := s.resumeHook
	s.mu.Unlock()
	if ok {
		s.hitlLog(ctx, "waiter woken", "approval_id", approvalID, "approved", approved)
		select {
		case ch <- answer{approved: approved}:
		default:
			// Should not happen (fresh channel, one winning Respond per
			// row), but never block Respond on a full channel.
		}
		return nil
	}

	// Waiter gone: the run suspended to a checkpoint. Resume it
	// synchronously; ErrNoCheckpoint is the clean no-op.
	if runHook && hook != nil {
		if err := hook(ctx, approvalID); err != nil && !errors.Is(err, ErrNoCheckpoint) {
			// The verdict is recorded either way; only the resume itself failed.
			s.hitlLog(ctx, "turn failed", "approval_id", approvalID, "reason", "resume_failed", "error", err.Error())
			return fmt.Errorf("hitlservice: verdict for approval %s recorded, but resuming its suspended run failed: %w", approvalID, err)
		}
		s.hitlLog(ctx, "tool resumed", "approval_id", approvalID, "approved", approved)
	}
	return nil
}

// sweepBatchLimit caps rows resolved per SweepExpired call so a large
// backlog can't block indefinitely; the next tick picks up the rest.
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
		// on this id in this process.
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
		// An expired approval may back a suspended run; resume it with the
		// timeout outcome. Best-effort: a resume failure is reported but
		// never fails the sweep.
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

// resumeExpired treats "nothing was suspended" as the clean no-op it is.
func (s *service) resumeExpired(ctx context.Context, hook ResumeHook, approvalID string) error {
	if err := hook(ctx, approvalID); err != nil && !errors.Is(err, ErrNoCheckpoint) {
		return err
	}
	return nil
}

// ListPending implements Service.ListPending. Unlike SweepExpired's silent
// no-op, it reports a missing durable store as an explicit error.
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

// onTimeoutOutcome mirrors localtools.HITLWrapper.Exec's on-timeout branch:
// only ActionAllow auto-approves. A validated policy can't set "allow" here
// (see validatePolicy), so this only ever observably returns false.
func onTimeoutOutcome(onTimeout Action) bool {
	return onTimeout == ActionAllow
}

// approvalResolution is the payload written into HITLApproval.Resolution. A
// permission ask writes only Approved; Answer/AnsweredBy serve attention asks.
type approvalResolution struct {
	Approved *bool `json:"approved,omitempty"`
	// Answer carries an attention ask's reply — the operator's own words.
	Answer *string `json:"answer,omitempty"`
	// AnsweredBy records who answered when not human (only "agent" today).
	AnsweredBy *string `json:"answeredBy,omitempty"`
}

// answer is what a parked requester is woken with: RequestApproval reads
// approved, RequestAttention reads text.
type answer struct {
	approved bool
	text     string
}

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

// AbandonMissionAsks implements Service; the CAS in ResolveHITLApproval
// keeps it race-safe against a concurrent Respond/Answer.
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
		// Wake a waiter parked on this instance — same best-effort shape as resolve().
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

// marshalApprovalResolution encodes approved as the resolution payload. The
// fallback exists only because callers shouldn't handle an encode error.
func marshalApprovalResolution(approved bool) json.RawMessage {
	raw, err := json.Marshal(approvalResolution{Approved: &approved})
	if err != nil {
		return json.RawMessage(fmt.Sprintf(`{"approved":%t}`, approved))
	}
	return raw
}

// summarizeApprovalArgs picks a human-recognizable field from a tool call's
// args, mirroring localtools' hitlArgsSummary. Duplicated to avoid an import cycle.
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
