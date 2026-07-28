// Package fleetservice is the fleet lifecycle-policy layer between the
// agent-instance kernel (agentinstance, policy-free) and its consumers: it
// decides whether an instance should be brought up, rolled back, or torn
// down. Every dispatch is a mission.
package fleetservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/contenox/beam/internal/errdefs"
	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/libacp"
	"github.com/google/uuid"
)

// DispatchRequest is the input to Dispatch. Intent and HITLPolicyName are
// both required: every dispatch is a mission. fleetapi's wire DTO is a type
// alias onto this.
type DispatchRequest struct {
	AgentName string `json:"agentName"`
	// Intent is sent as the unit's first turn.
	Intent string `json:"intent"`
	// HITLPolicyName names the mission's envelope; not defaulted from config.
	HITLPolicyName string `json:"hitlPolicyName"`
	Cwd            string `json:"cwd,omitempty"`

	// ParentSessionID names the upstream session firing this mission. Empty
	// when an operator fires directly, routing reports to the inbox instead.
	ParentSessionID string `json:"parentSessionId,omitempty"`
}

// DispatchResult is Dispatch's output. MissionID is always present.
type DispatchResult struct {
	InstanceID string `json:"instanceId"`
	SessionID  string `json:"sessionId"`
	MissionID  string `json:"missionId"`
}

// Service is the fleet's operational surface: read the board (List/Get),
// allocate a unit (Dispatch), and end one (Stop/Cancel).
type Service interface {
	// List returns the config+runtime join: every declared agent annotated
	// with its live instances.
	List(ctx context.Context) ([]agentinstance.FleetEntry, error)

	// Get returns one instance's status, or agentinstance.ErrNotFound.
	Get(ctx context.Context, instanceID string) (agentinstance.InstanceStatus, error)

	// Dispatch allocates a unit past the fleet-width cap (see admission.go),
	// records a mission, and runs the intent as its first turn on a detached
	// context, returning once the session is open. It also shepherds the
	// turn's outcome: liveness on every completed turn, one nudge if mute,
	// then a blocker (see driveUnattendedMission). Any failure after Start
	// tears the fresh instance back down.
	Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error)

	// Stop tears instanceID down; idempotent, per kernel contract.
	Stop(ctx context.Context, instanceID string) error

	// Cancel cancels an in-flight prompt turn: exactly sessionID if given,
	// or every session on instanceID if empty.
	Cancel(ctx context.Context, instanceID, sessionID string) error
}

type service struct {
	instances      agentinstance.Manager
	agents         agentregistryservice.Service
	missions       missionservice.Service
	workspaceRoots *vfs.Factory
	projectRoot    string
	tracker        libtracker.ActivityTracker
	// computeBounds reads maxTurns/maxTokens for the drive loop. Nil leaves
	// those seams unbounded.
	computeBounds hitlservice.ComputeBoundsReader
	// policyValidator refuses a dispatch naming a nonexistent HITL envelope.
	// Nil skips the check.
	policyValidator hitlservice.PolicyValidator
	// maxParallel is the fleet-width admission cap; 0 means unlimited.
	maxParallel int
	// admission serializes the cap's count-then-allocate window: held from
	// admitUnit through StartResolved.
	admission sync.Mutex
}

// Option configures a fleet Service at construction.
type Option func(*service)

// WithPolicyValidator wires the creation-time envelope existence check.
func WithPolicyValidator(v hitlservice.PolicyValidator) Option {
	return func(s *service) { s.policyValidator = v }
}

// WithComputeBounds wires the reader the drive loop consults for maxTurns
// and maxTokens. maxToolCalls is enforced separately by the answerer.
func WithComputeBounds(r hitlservice.ComputeBoundsReader) Option {
	return func(s *service) { s.computeBounds = r }
}

// New returns a Service driving instances (the kernel) and agents. An absent
// cwd defaults to projectRoot (see resolveCwd). missions is not optional. A
// nil tracker degrades to a Noop.
func New(
	instances agentinstance.Manager,
	agents agentregistryservice.Service,
	missions missionservice.Service,
	workspaceRoots *vfs.Factory,
	projectRoot string,
	tracker libtracker.ActivityTracker,
	opts ...Option,
) Service {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	s := &service{
		instances:      instances,
		agents:         agents,
		missions:       missions,
		workspaceRoots: workspaceRoots,
		projectRoot:    projectRoot,
		tracker:        tracker,
		// Enforced before any option runs; WithMaxParallel only changes it.
		maxParallel: DefaultMaxParallel,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *service) List(ctx context.Context) ([]agentinstance.FleetEntry, error) {
	return s.instances.List(ctx)
}

func (s *service) Get(ctx context.Context, instanceID string) (agentinstance.InstanceStatus, error) {
	_ = ctx // agentinstance.Manager.Get is purely in-memory; ctx governs nothing here.
	return s.instances.Get(instanceID)
}

func (s *service) Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	if strings.TrimSpace(req.AgentName) == "" {
		return DispatchResult{}, errdefs.MissingParameter("agentName", "agentName is required")
	}
	if strings.TrimSpace(req.Intent) == "" {
		return DispatchResult{}, errdefs.MissingParameter("intent", "intent is required")
	}
	if strings.TrimSpace(req.HITLPolicyName) == "" {
		return DispatchResult{}, errdefs.MissingParameter("hitlPolicyName", "hitlPolicyName is required: a mission must name its envelope")
	}
	// Validate the named policy loads before anything is brought up; the
	// drive loop's own bounds read stays fail-to-unbounded.
	if s.policyValidator != nil {
		if err := s.policyValidator.ValidatePolicy(ctx, req.HITLPolicyName); err != nil {
			return DispatchResult{}, errdefs.InvalidParameterValue("hitlPolicyName",
				fmt.Sprintf("hitl policy %q could not be loaded: it must name an existing policy (see `contenox config` / the .contenox policy files)", strings.TrimSpace(req.HITLPolicyName)))
		}
	}
	cwd, err := s.resolveCwd(req.Cwd)
	if err != nil {
		return DispatchResult{}, err
	}

	// Refuse a disabled agent before bringing anything up; the kernel
	// itself has no concept of Enabled.
	agent, err := agentregistryservice.ResolveForSpawn(ctx, s.agents, req.AgentName)
	if err != nil {
		if errors.Is(err, agentregistryservice.ErrAgentDisabled) {
			return DispatchResult{}, errdefs.Conflict(err.Error())
		}
		return DispatchResult{}, err
	}

	// Generated before the session is opened: the unit must hold it at
	// construction (forwarded as session/new `_meta`). The mission row
	// itself is written later, after the session opens.
	missionID := uuid.NewString()

	// StartResolved, not Start(agentName): Start would re-read the row, a
	// TOCTOU window where an agent disabled between the two reads would
	// still spawn. The lock spans the check and the allocation so concurrent
	// dispatches cannot slip past the cap between counting and spawning.
	s.admission.Lock()
	if err := s.admitUnit(ctx); err != nil {
		s.admission.Unlock()
		return DispatchResult{}, err
	}
	instanceID, err := s.instances.StartResolved(ctx, agent, cwd)
	s.admission.Unlock()
	if err != nil {
		return DispatchResult{}, err
	}

	// The mission id and the envelope's model/backend allowlist both ride
	// session/new `_meta` (missionservice.ParseMissionMeta), since model
	// choice happens inside the unit's own process. On failure tear the
	// fresh instance down so a failed dispatch never leaks a subprocess.
	dispatchBounds := s.dispatchResolutionBounds(ctx, req.HITLPolicyName)
	sessionID, err := s.instances.OpenSession(ctx, instanceID, agentinstance.SessionSpec{
		Cwd:  cwd,
		Meta: missionservice.MarshalMissionMetaBounded(missionID, dispatchBounds.ModelAllowlist, dispatchBounds.BackendAllowlist),
	})
	if err != nil {
		_ = s.instances.Stop(instanceID)
		return DispatchResult{}, err
	}

	result := DispatchResult{InstanceID: instanceID, SessionID: string(sessionID)}

	m := &missionservice.Mission{
		ID:              missionID,
		Intent:          req.Intent,
		AgentName:       req.AgentName,
		HITLPolicyName:  req.HITLPolicyName,
		ParentSessionID: req.ParentSessionID,
	}
	if err := s.missions.Create(ctx, m); err != nil {
		_ = s.instances.Stop(instanceID)
		return DispatchResult{}, err
	}
	if _, err := s.missions.Bind(ctx, m.ID, string(sessionID), instanceID); err != nil {
		_ = s.instances.Stop(instanceID)
		return DispatchResult{}, err
	}
	result.MissionID = m.ID

	// The mission's turns run detached; context.WithoutCancel keeps
	// request-scoped values while surviving the caller's return.
	recordDispatch()

	detached := context.WithoutCancel(ctx)
	go s.driveUnattendedMission(detached, missionRun{
		instanceID: instanceID,
		sessionID:  sessionID,
		missionID:  m.ID,
		agentName:  req.AgentName,
		intent:     req.Intent,
	})

	return result, nil
}

// missionRun is the fully-resolved input to the detached goroutine that
// shepherds a dispatched unit's turns.
type missionRun struct {
	instanceID string
	sessionID  libacp.SessionID
	missionID  string
	agentName  string
	intent     string
}

// driveUnattendedMission runs a dispatched unit's turns and shepherds their
// outcomes: liveness on every completed turn, one nudge if mute, then a
// runtime-filed blocker. The nudge loop is hard-capped at one turn.
func (s *service) driveUnattendedMission(ctx context.Context, run missionRun) {
	reportErr, reportChange, end := s.tracker.Start(ctx, "prompt", "fleet_dispatch",
		"instance_id", run.instanceID, "session_id", string(run.sessionID), "agent_name", run.agentName)
	defer end()

	bounds := s.computeBoundsFor(ctx, run.missionID)

	// Turn 1: the mission preamble ahead of the clean intent.
	firstTurn := []libacp.ContentBlock{
		libacp.NewTextContent(missionPreamble),
		libacp.NewTextContent(run.intent),
	}
	stop, err := s.promptTurn(ctx, run, firstTurn)
	if err != nil {
		reportErr(err)
		return
	}
	reportChange(string(run.sessionID), string(stop))
	if s.missionReached(ctx, run.missionID) {
		return // the unit's voice reached the operator; nothing to correct.
	}
	if s.enforceTokenBudget(ctx, run, bounds, reportChange) {
		return // the mission spent its token budget on turn 1; finished stuck.
	}

	// The one bounded nudge runs only if the turn budget permits it.
	if turnBudgetExceeded(2, bounds) {
		s.finishComputeStuck(ctx, run.missionID, turnsExhaustedReason(bounds), reportChange)
		return
	}
	stop, err = s.promptTurn(ctx, run, []libacp.ContentBlock{libacp.NewTextContent(missionNudge)})
	if err != nil {
		reportErr(err)
		return
	}
	reportChange(string(run.sessionID), string(stop))
	if s.missionReached(ctx, run.missionID) {
		return // the nudge worked.
	}
	if s.enforceTokenBudget(ctx, run, bounds, reportChange) {
		return // spent its token budget across the two turns; finished stuck.
	}

	// Mute across both turns: the runtime files the blocker itself. No third
	// prompt, ever.
	s.fileSilentTurnBlocker(ctx, run)
}

// computeBoundsFor reads the mission's compute ceiling, or zero (unbounded)
// when no reader is wired or the read fails.
func (s *service) computeBoundsFor(ctx context.Context, missionID string) hitlservice.ComputeBounds {
	if s.computeBounds == nil {
		return hitlservice.ComputeBounds{}
	}
	m, err := s.missions.Get(ctx, missionID)
	if err != nil || m == nil {
		return hitlservice.ComputeBounds{}
	}
	bounds, err := s.computeBounds.ComputeBoundsFor(ctx, m.HITLPolicyName)
	if err != nil {
		return hitlservice.ComputeBounds{}
	}
	return bounds
}

// dispatchResolutionBounds reads policyName's allowlists before the mission
// row exists. Unbounded on any failure.
func (s *service) dispatchResolutionBounds(ctx context.Context, policyName string) hitlservice.ComputeBounds {
	if s.computeBounds == nil {
		return hitlservice.ComputeBounds{}
	}
	bounds, err := s.computeBounds.ComputeBoundsFor(ctx, policyName)
	if err != nil {
		return hitlservice.ComputeBounds{}
	}
	return bounds
}

// enforceTokenBudget stops a mission that has spent its token budget. Runs
// only between turns; never cancels a turn in flight.
func (s *service) enforceTokenBudget(ctx context.Context, run missionRun, b hitlservice.ComputeBounds, reportChange func(string, any)) bool {
	if b.MaxTokens <= 0 {
		return false
	}
	notes, ok := s.sessionJournal(run)
	if !ok {
		return false
	}
	used, present := journalTokenUsage(notes)
	if !present || !tokenBudgetExceeded(used, b) {
		return false
	}
	s.finishComputeStuck(ctx, run.missionID, tokensExhaustedReason(b, used), reportChange)
	return true
}

// finishComputeStuck brings a mission to rest at StatusStuck, naming the
// bound it crossed. A conflicting Finish (already terminal) is recorded and
// dropped; the durable status is correct either way.
func (s *service) finishComputeStuck(ctx context.Context, missionID, reason string, reportChange func(string, any)) {
	reportChange("compute_bound", reason)
	if _, err := s.missions.Finish(ctx, missionID, missionservice.StatusStuck, reason); err != nil {
		reportChange("compute_bound_finish_error", err.Error())
	}
}

// sessionJournalReader is the optional kernel capability the drive loop uses
// to read reported token usage without attaching a viewer.
type sessionJournalReader interface {
	SessionJournal(instanceID string, sessionID libacp.SessionID) ([]libacp.SessionNotification, string, bool)
}

func (s *service) sessionJournal(run missionRun) ([]libacp.SessionNotification, bool) {
	reader, ok := s.instances.(sessionJournalReader)
	if !ok {
		return nil, false
	}
	notes, _, owned := reader.SessionJournal(run.instanceID, run.sessionID)
	return notes, owned
}

// promptTurn drives one detached turn and stamps mission liveness from its
// outcome. The heartbeat write's own error is deliberately dropped.
func (s *service) promptTurn(ctx context.Context, run missionRun, blocks []libacp.ContentBlock) (libacp.StopReason, error) {
	stop, err := s.instances.Prompt(ctx, run.instanceID, run.sessionID, blocks)
	if err != nil {
		_, _ = s.missions.Heartbeat(ctx, run.missionID, err.Error())
		return "", err
	}
	_, _ = s.missions.Heartbeat(ctx, run.missionID, "")
	return stop, nil
}

// missionReached reports whether the mission carries any fact a unit
// produced through its mission tools, read off the durable mission store.
func (s *service) missionReached(ctx context.Context, missionID string) bool {
	m, err := s.missions.Get(ctx, missionID)
	if err != nil {
		m = nil // not evidence the unit reached anyone.
	}
	reportCount := 0
	if reports, rerr := s.missions.ListReports(ctx, missionID, 1); rerr == nil {
		reportCount = len(reports)
	}
	return missionShowsUnitReached(m, reportCount)
}

// missionShowsUnitReached reports whether the unit reached the operator: a
// filed report, a terminal verdict, or a plan revision each count. It does
// not see an attention ask raised through the approval store; the worst
// case is one harmless extra nudge.
func missionShowsUnitReached(m *missionservice.Mission, reportCount int) bool {
	if reportCount > 0 {
		return true
	}
	if m == nil {
		return false
	}
	if m.Status != missionservice.StatusOpen {
		return true // mission_finish recorded a terminal verdict
	}
	return m.Plan.Revision > 0 // mission_plan revised the living plan
}

// fileSilentTurnBlocker files the runtime's own blocker for a unit that
// ended two turns without reporting, quoting its last words when cheaply
// recoverable (see sessionTextReader).
func (s *service) fileSilentTurnBlocker(ctx context.Context, run missionRun) {
	summary, detail := silentTurnBlocker(s.lastAgentText(run.instanceID, run.sessionID), string(run.sessionID))
	_ = s.missions.AddReport(ctx, run.missionID, &missionservice.Report{
		Kind:    missionservice.ReportKindBlocker,
		Summary: summary,
		Detail:  detail,
	})
}

// sessionTextReader is the optional capability used to quote a silent unit's
// own words without attaching a viewer.
type sessionTextReader interface {
	SessionAgentText(instanceID string, sessionID libacp.SessionID) (string, bool)
}

func (s *service) lastAgentText(instanceID string, sessionID libacp.SessionID) string {
	reader, ok := s.instances.(sessionTextReader)
	if !ok {
		return ""
	}
	text, _ := reader.SessionAgentText(instanceID, sessionID)
	return text
}

// Derived from the tool package's own constants, qualified with the provider
// name exactly as taskengine offers them to the model.
var (
	toolAskAttention = missiontools.ToolsProviderName + "." + missiontools.ToolNameAskAttention
	toolReport       = missiontools.ToolsProviderName + "." + missiontools.ToolNameReport
	toolFinish       = missiontools.ToolsProviderName + "." + missiontools.ToolNameFinish
)

// missionPreamble is prepended to a unit's first turn. Wire-only: never
// persisted as Mission.Intent.
var missionPreamble = fmt.Sprintf(`You are running as an UNATTENDED mission unit. No human is reading this conversation — replying in prose reaches no one. You reach your operator ONLY through your mission tools:
- %s: ask a question, or flag a blocker you must not decide alone.
- %s: record real progress, a finding, or a result.
- %s: end the mission with a verdict, once the work is truly done.
Do the work with your other tools. When you need the operator, or have something worth their attention, call a mission tool. Chat text alone will not be seen.`, toolAskAttention, toolReport, toolFinish)

// missionNudge is the single follow-up turn a mute unit earns (see
// driveUnattendedMission's hard cap).
var missionNudge = fmt.Sprintf(`Your last turn ended without reaching your operator, and no human is reading this chat. To reach your operator now, call %s (a question or a blocker) or %s (progress, a finding, or a result); to end the mission, call %s. If you are not done, keep working with your other tools and report when you have something. Do not answer in prose alone — it will not be seen.`, toolAskAttention, toolReport, toolFinish)

// silentTurnBlockerLead is the stable prefix of a mute unit's blocker.
const silentTurnBlockerLead = "unit ended two turns without reporting"

// silentTurnBlocker builds the (summary, detail) of the blocker filed on a
// mute unit's behalf. summary is always single-line and non-empty, as
// missionservice.AddReport requires.
func silentTurnBlocker(lastAgentText, sessionID string) (summary, detail string) {
	attach := fmt.Sprintf("The unit produced no mission report across two turns (its first turn and one runtime nudge). Attach to session %s to read its transcript and continue it.", sessionID)
	lastAgentText = strings.TrimSpace(lastAgentText)
	if lastAgentText == "" {
		return fmt.Sprintf("%s; attach to session %s", silentTurnBlockerLead, sessionID), attach
	}
	summary = singleLineExcerpt(fmt.Sprintf("%s — last said: %s", silentTurnBlockerLead, lastAgentText), 240)
	detail = fmt.Sprintf("The unit's last words:\n\n%s\n\n%s", lastAgentText, attach)
	return summary, detail
}

// singleLineExcerpt collapses all whitespace to single spaces and truncates
// to max runes with an ellipsis.
func singleLineExcerpt(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}

func (s *service) Stop(ctx context.Context, instanceID string) error {
	_ = ctx // agentinstance.Manager.Stop is purely in-memory; ctx governs nothing here.
	return s.instances.Stop(instanceID)
}

func (s *service) Cancel(ctx context.Context, instanceID, sessionID string) error {
	_ = ctx // agentinstance.Manager.Cancel/Get take no ctx; kept for interface uniformity.
	if sessionID != "" {
		return s.instances.Cancel(instanceID, libacp.SessionID(sessionID))
	}
	// No session named: cancel every session attached to the instance. Safe
	// with no turn in flight; zero sessions is a no-op.
	status, err := s.instances.Get(instanceID)
	if err != nil {
		return err
	}
	var errs []error
	for _, sid := range status.SessionIDs {
		if cerr := s.instances.Cancel(instanceID, libacp.SessionID(sid)); cerr != nil {
			errs = append(errs, cerr)
		}
	}
	return errors.Join(errs...)
}

// resolveCwd maps a requested session cwd onto the concrete root the
// dispatched unit will run in, via vfs.ResolveSessionCwd. An absent cwd
// defaults to the workspace Factory's default root; s.projectRoot is used
// only as a fallback when no allowlist is configured. A relative cwd is
// refused.
func (s *service) resolveCwd(cwd string) (string, error) {
	resolved, err := vfs.ResolveSessionCwd(s.workspaceRoots, cwd, s.projectRoot)
	if err != nil {
		return "", errdefs.InvalidParameterValue("cwd", err.Error())
	}
	return resolved, nil
}
