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

	"github.com/contenox/contenox/errdefs"
	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
)

// DispatchRequest is the input to Dispatch; Intent and HITLPolicyName are required, and fleetapi's wire DTO is a type alias onto this.
type DispatchRequest struct {
	AgentName string `json:"agentName"`
	// Intent is sent as the unit's first turn.
	Intent string `json:"intent"`
	// HITLPolicyName names the mission's envelope; not defaulted from config.
	HITLPolicyName string `json:"hitlPolicyName"`
	Cwd            string `json:"cwd,omitempty"`

	// ParentSessionID names the upstream session firing this mission; empty when an operator fires directly, routing reports to the inbox instead.
	ParentSessionID string `json:"parentSessionId,omitempty"`
}

// DispatchResult is Dispatch's output; MissionID is always present.
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

	// Dispatch allocates a unit past the fleet-width cap, records a mission, runs the intent as its first turn on a detached context, shepherds the turn's outcome to a nudge-then-blocker or derailed finish, and tears the fresh instance back down on any failure after Start.
	Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error)

	// Stop tears instanceID down; idempotent, per kernel contract.
	Stop(ctx context.Context, instanceID string) error

	// Cancel cancels an in-flight prompt turn: exactly sessionID if given,
	// or every session on instanceID if empty.
	Cancel(ctx context.Context, instanceID, sessionID string) error
}

type service struct {
	instances       agentinstance.Manager
	agents          agentregistryservice.Service
	missions        missionservice.Service
	workspaceRoots  *vfs.Factory
	projectRoot     string
	tracker         libtracker.ActivityTracker
	computeBounds   hitlservice.ComputeBoundsReader
	policyValidator hitlservice.PolicyValidator
	guidance        guidanceReader
	maxParallel     int
	admission       sync.Mutex
}

// Option configures a fleet Service at construction.
type Option func(*service)

// WithPolicyValidator wires the creation-time envelope existence check.
func WithPolicyValidator(v hitlservice.PolicyValidator) Option {
	return func(s *service) { s.policyValidator = v }
}

// WithComputeBounds wires the reader the drive loop consults for maxTurns and maxTokens; maxToolCalls is enforced separately by the answerer.
func WithComputeBounds(r hitlservice.ComputeBoundsReader) Option {
	return func(s *service) { s.computeBounds = r }
}

// WithAdjudicationGuidance wires the reader the nudge turn consults for the redirects an adjudicating agent left on refused calls.
func WithAdjudicationGuidance(r guidanceReader) Option {
	return func(s *service) { s.guidance = r }
}

// New returns a Service driving instances and agents; missions is required, a nil tracker degrades to a Noop, and an absent cwd defaults to projectRoot (see resolveCwd).
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
		maxParallel:    DefaultMaxParallel,
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

	// Generated before the session opens: the unit must hold it at construction (forwarded as session/new `_meta`); the mission row itself is written later.
	missionID := uuid.NewString()

	// StartResolved (not Start): re-reading the row would open a TOCTOU window letting a newly disabled agent spawn; the lock spans admitUnit and StartResolved so concurrent dispatches cannot both pass the cap.
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

	// mission id and allowlist ride session/new `_meta`; on failure the fresh instance is torn down so a failed dispatch never leaks a subprocess.
	dispatchBounds := s.dispatchResolutionBounds(ctx, req.HITLPolicyName)
	sessionID, err := s.instances.OpenSession(ctx, instanceID, agentinstance.SessionSpec{
		Cwd:  cwd,
		Meta: missionservice.MarshalMissionMetaBounded(missionID, dispatchBounds.ModelAllowlist, dispatchBounds.BackendAllowlist, req.HITLPolicyName),
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

func (s *service) driveUnattendedMission(ctx context.Context, run missionRun) {
	reportErr, reportChange, end := s.tracker.Start(ctx, "prompt", "fleet_dispatch",
		"instance_id", run.instanceID, "session_id", string(run.sessionID), "agent_name", run.agentName)
	defer end()

	bounds := s.computeBoundsFor(ctx, run.missionID)

	firstTurn := []libacp.ContentBlock{
		libacp.NewTextContent(missionPreamble),
		libacp.NewTextContent(run.intent),
	}
	stop, err := s.promptTurn(ctx, run, firstTurn)
	if err != nil {
		s.failTurn(ctx, run, err, reportErr, reportChange)
		return
	}
	reportChange(string(run.sessionID), string(stop))
	if s.missionReached(ctx, run.missionID) {
		return
	}
	if s.enforceTokenBudget(ctx, run, bounds, reportChange) {
		return
	}

	if turnBudgetExceeded(2, bounds) {
		s.finishComputeStuck(ctx, run.missionID, turnsExhaustedReason(bounds), reportChange)
		return
	}
	stop, err = s.promptTurn(ctx, run, []libacp.ContentBlock{libacp.NewTextContent(s.nudgeText(ctx, run.missionID))})
	if err != nil {
		s.failTurn(ctx, run, err, reportErr, reportChange)
		return
	}
	reportChange(string(run.sessionID), string(stop))
	if s.missionReached(ctx, run.missionID) {
		return
	}
	if s.enforceTokenBudget(ctx, run, bounds, reportChange) {
		return
	}

	// Mute across both turns: no third prompt, ever — the runtime files the blocker itself.
	s.fileSilentTurnBlocker(ctx, run)
}

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

func (s *service) finishComputeStuck(ctx context.Context, missionID, reason string, reportChange func(string, any)) {
	reportChange("compute_bound", reason)
	if _, err := s.missions.Finish(ctx, missionID, missionservice.StatusStuck, reason); err != nil {
		reportChange("compute_bound_finish_error", err.Error())
	}
}

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

func (s *service) promptTurn(ctx context.Context, run missionRun, blocks []libacp.ContentBlock) (libacp.StopReason, error) {
	stop, err := s.instances.Prompt(ctx, run.instanceID, run.sessionID, blocks)
	if err != nil {
		return "", err
	}
	_, _ = s.missions.Heartbeat(ctx, run.missionID, "")
	return stop, nil
}

func (s *service) failTurn(ctx context.Context, run missionRun, turnErr error, reportErr func(error), reportChange func(string, any)) {
	reportErr(turnErr)
	defer func() {
		if err := s.instances.Stop(run.instanceID); err != nil {
			reportChange("turn_error_stop_error", err.Error())
		}
	}()

	if m, err := s.missions.Get(ctx, run.missionID); err == nil && m != nil && m.Status != missionservice.StatusOpen {
		reportChange("turn_error", fmt.Sprintf("mission already at rest as %s; unit reaped", m.Status))
		return
	}

	reason := turnErrorLine(turnErr)
	reportChange("turn_error", reason)
	if err := s.missions.AddReport(ctx, run.missionID, &missionservice.Report{
		Kind:    missionservice.ReportKindBlocker,
		Summary: reason,
		Detail:  turnErrorDetail(turnErr, string(run.sessionID)),
	}); err != nil {
		reportChange("turn_error_report_error", err.Error())
	}
	if _, err := s.missions.Finish(ctx, run.missionID, missionservice.StatusDerailed, reason); err != nil {
		reportChange("turn_error_finish_error", err.Error())
	}
}

const turnErrorBlockerLead = "unit turn failed"

func turnErrorLine(turnErr error) string {
	return singleLineExcerpt(fmt.Sprintf("%s: %v", turnErrorBlockerLead, turnErr), 240)
}

func turnErrorDetail(turnErr error, sessionID string) string {
	return fmt.Sprintf(
		"The unit's turn on session %s returned an error, and the drive loop is the only thing prompting a dispatched unit:\n\n%v\n\n"+
			"The mission was finished %s and the unit stopped, so it no longer holds fleet width. Re-dispatch once the cause is addressed.",
		sessionID, turnErr, missionservice.StatusDerailed)
}

func (s *service) missionReached(ctx context.Context, missionID string) bool {
	m, err := s.missions.Get(ctx, missionID)
	if err != nil {
		m = nil
	}
	reportCount := 0
	if reports, rerr := s.missions.ListReports(ctx, missionID, 1); rerr == nil {
		reportCount = len(reports)
	}
	return missionShowsUnitReached(m, reportCount)
}

func missionShowsUnitReached(m *missionservice.Mission, reportCount int) bool {
	if reportCount > 0 {
		return true
	}
	if m == nil {
		return false
	}
	if m.Status != missionservice.StatusOpen {
		return true
	}
	return m.Plan.Revision > 0
}

func (s *service) fileSilentTurnBlocker(ctx context.Context, run missionRun) {
	summary, detail := silentTurnBlocker(s.lastAgentText(run.instanceID, run.sessionID), string(run.sessionID))
	_ = s.missions.AddReport(ctx, run.missionID, &missionservice.Report{
		Kind:    missionservice.ReportKindBlocker,
		Summary: summary,
		Detail:  detail,
	})
}

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

var (
	toolReport = missiontools.ToolsProviderName + "." + missiontools.ToolNameReport
	toolAsk    = missiontools.ToolsProviderName + "." + missiontools.ToolNameAskAttention
	toolFinish = missiontools.ToolsProviderName + "." + missiontools.ToolNameFinish
)

var missionPreamble = fmt.Sprintf(`You are running as an UNATTENDED mission unit. No human is reading this conversation — replying in prose reaches no one. You reach outside this session ONLY through your mission tools:
- %s: record real progress, a finding, a blocker, or a result.
- %s: ask for a decision you may not make alone, and WAIT for the reply. It comes back as this tool's result, so you continue with it on the same turn. Nobody may be listening — if the reply does not come, your question is filed as a blocker and you carry on.
- %s: end the mission with a verdict, once the work is truly done.
Do the work with your other tools. Decide what you can from the intent you were given; ask only for what the intent genuinely does not settle. Chat text alone will not be seen.`, toolReport, toolAsk, toolFinish)

var missionNudge = fmt.Sprintf(`Your last turn ended without reaching outside this session, and no human is reading this chat. To reach your operator now, call %s (progress, a finding, a blocker, or a result); if you are blocked on a decision you may not make alone, call %s and wait for the reply; to end the mission, call %s. If you are not done, keep working with your other tools and report when you have something. Do not answer in prose alone — it will not be seen.`, toolReport, toolAsk, toolFinish)

// guidanceReader is the optional adjudication half of hitlservice; a host that
// wired no adjudicator satisfies it with nothing to report.
type guidanceReader interface {
	AgentGuidanceFor(ctx context.Context, missionID string) ([]hitlservice.GuidanceNote, error)
}

// nudgeText prefixes the nudge with why the unit's calls were refused. A
// refusal it never saw a reason for is the thing that leaves it circling.
func (s *service) nudgeText(ctx context.Context, missionID string) string {
	if s.guidance == nil {
		return missionNudge
	}
	notes, err := s.guidance.AgentGuidanceFor(ctx, missionID)
	if err != nil || len(notes) == 0 {
		return missionNudge
	}
	var b strings.Builder
	b.WriteString("Some of your tool calls were refused, each with a redirect you did not see:\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "- %s.%s was refused: %s\n", n.ToolsName, n.ToolName, n.Guidance)
	}
	b.WriteString("\nTake those redirects and continue. ")
	b.WriteString(missionNudge)
	return b.String()
}

const silentTurnBlockerLead = "unit ended two turns without reporting"

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

func (s *service) resolveCwd(cwd string) (string, error) {
	resolved, err := vfs.ResolveSessionCwd(s.workspaceRoots, cwd, s.projectRoot)
	if err != nil {
		return "", errdefs.InvalidParameterValue("cwd", err.Error())
	}
	return resolved, nil
}
