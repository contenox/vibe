// Package fleetservice is the fleet lifecycle-POLICY layer sitting between the
// agent-instance kernel (runtime/agentinstance) and its consumers. The kernel
// is deliberately policy-free — agentinstance.Manager knows HOW to bring an
// instance up, drive a session, and tear one down, but never WHETHER it
// should: that judgment (refuse a disabled agent, roll back a half-dispatched
// unit, fan a session-less cancel out over every open session) has to live
// somewhere, and scattering it across every caller lets the callers drift
// apart. This package is that one place.
//
// It is the service-package idiom this codebase already uses for the fleet's
// durable half (runtime/missionservice): a validated interface over ctx +
// error, a New() constructor, no HTTP concerns. Today's only consumer is
// runtime/internal/fleetapi (thin REST handlers over this Service); the
// `contenox fleet` CLI (a follow-up slice) mounts on the same interface
// instead of re-deriving the orchestration. See
// docs/development/blueprints/beam/fleet-manager.md for the fleet-manager
// ontology (manifest, dispatch, envelopes, telemetry) this layer implements
// the "dispatch" half of.
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

// DispatchRequest is the input to Dispatch: the declared agent to bring up,
// the intent that becomes its first turn, the HITL policy that bounds it
// while it runs unattended, and an optional session working directory
// (validated against the workspace-root allowlist). It is the single source
// of truth for the shape — fleetapi's wire DTO is a type alias onto this, not
// an independent copy.
//
// Every dispatch is a mission (docs/development/blueprints/acp/
// fleet-consolidation.md, "Mission mode"): there is no headless bring-up that
// is not one, so Intent and HITLPolicyName are both required rather than
// optional extras layered onto a separate prompt. The intent IS the prompt —
// it is sent as the unit's first turn (Dispatch step 4) — so there is no
// separate prompt field to also populate.
type DispatchRequest struct {
	AgentName string `json:"agentName"`
	// Intent is the one-line mission intent: what the unit is being sent to
	// do, and also the content of its first turn. Required — a dispatch with
	// nothing to do is not a dispatch.
	Intent string `json:"intent"`
	// HITLPolicyName names the HITL policy that becomes the mission's
	// envelope — what bounds the unit while it runs unattended (see
	// missionservice.Mission). Required: a mission with no envelope is a
	// mission with no bounds, which mission mode must not permit.
	//
	// Deliberately NOT defaulted from config here: fleetservice has no
	// config dependency, and adding one only to backfill this default would
	// be scope creep for what this slice is doing. A later slice prefills
	// this field in beam's dispatch form instead. Its absence from this
	// struct's defaulting logic is a decision, not an oversight.
	HITLPolicyName string `json:"hitlPolicyName"`
	Cwd            string `json:"cwd,omitempty"`

	// ParentSessionID names the UPSTREAM session firing this mission — the
	// supervision edge (see missionservice.Mission.ParentSessionID). It is set
	// when one agent's session fires a mission from within a conversation (the
	// `/mission` slash command), so the fired unit's reports can reach the caller
	// that can act on them, and left empty when an operator fires directly, which
	// routes reports to the operator inbox instead.
	//
	// Optional and unvalidated on purpose: this layer records the edge, it does
	// not police it. runtime/reportrouter consumes it on report-add; this field
	// is populated where the information exists and empty everywhere else, rather
	// than being backfilled with a guess.
	ParentSessionID string `json:"parentSessionId,omitempty"`
}

// DispatchResult is Dispatch's output: the ids the dispatch created. MissionID
// is always present — every dispatch is a mission (see DispatchRequest).
type DispatchResult struct {
	InstanceID string `json:"instanceId"`
	SessionID  string `json:"sessionId"`
	MissionID  string `json:"missionId"`
}

// Service is the fleet's operational surface: read the board (List/Get),
// allocate a unit (Dispatch), and end one (Stop/Cancel). Every method takes a
// ctx and returns an error, even where the kernel method it wraps does not,
// so this seam stays uniform regardless of what agentinstance.Manager needs
// under it.
type Service interface {
	// List returns the config+runtime join: every declared agent, annotated
	// with its live instances. A thin passthrough to
	// agentinstance.Manager.List.
	List(ctx context.Context) ([]agentinstance.FleetEntry, error)

	// Get returns one instance's status, or agentinstance.ErrNotFound if
	// instanceID is unknown. A thin passthrough to agentinstance.Manager.Get.
	Get(ctx context.Context, instanceID string) (agentinstance.InstanceStatus, error)

	// Dispatch allocates a unit: it resolves and validates the declared
	// agent (refusing a disabled one), admits it past the fleet-width cap
	// (refusing with a teaching message when maxParallel units are already
	// open — see admission.go), brings up an instance, opens a session,
	// records a mission bound to both ids and carrying the request's
	// envelope, and runs the intent as the unit's first turn on a detached
	// context, returning as soon as the session is open
	// (async-after-OpenSession; the turn's outcome is observable on the
	// board).
	//
	// It is allocation, not operation — with one deliberate amendment this layer
	// now owns: the OUTCOME of the turns it starts, far enough to guarantee the
	// unit's voice can reach the operator. A detached first turn that ended in a
	// clarifying question once went NOWHERE — no heartbeat, no report, the
	// subprocess parked on stdin, the mission frozen at open and the operator
	// blind — and "allocation" that leaves a unit talking into the void is not
	// allocation, it is a silent death. So Dispatch stamps liveness on every
	// completed turn, teaches a mute first turn to use its mission tools with
	// exactly ONE nudge, and files a blocker FOR a unit still mute after it (see
	// driveUnattendedMission). It is still not operation in the larger sense: no
	// restart policy, no adoption into a beam chat session (a documented v1
	// limitation), and the nudge loop is hard-capped at one. Any failure after
	// Start tears the fresh instance back down so a failed dispatch never leaks a
	// running subprocess.
	Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error)

	// Stop tears instanceID down via agentinstance.Manager.Stop, which is
	// idempotent by kernel contract: stopping an unknown or already-stopped
	// id is a no-op returning nil, not an error. Callers (including a
	// DELETE /fleet/{id} handler) may therefore call Stop without a
	// preceding existence check.
	Stop(ctx context.Context, instanceID string) error

	// Cancel cancels an in-flight prompt turn. With sessionID given it
	// cancels exactly that session (agentinstance.Manager.Cancel is safe with
	// no turn in flight, so this is safe to call speculatively). With
	// sessionID empty it fans out over every session InstanceStatus.SessionIDs
	// reports for instanceID and cancels each — "stop everything running on
	// this instance" without the caller having to enumerate sessions itself.
	// Returns agentinstance.ErrNotFound for an unknown instanceID.
	Cancel(ctx context.Context, instanceID, sessionID string) error
}

type service struct {
	instances      agentinstance.Manager
	agents         agentregistryservice.Service
	missions       missionservice.Service
	workspaceRoots *vfs.Factory
	projectRoot    string
	tracker        libtracker.ActivityTracker
	// computeBounds reads a mission envelope's compute ceiling for the two bounds
	// the DRIVE LOOP enforces (maxTurns between turns, maxTokens from reported
	// usage). Nil leaves those seams unbounded — today's behavior — so a
	// fleetservice built without WithComputeBounds behaves exactly as before compute
	// bounds existed. maxToolCalls is enforced by the unattended answerer instead,
	// which reads its own bounds from the HITL service it already holds.
	computeBounds hitlservice.ComputeBoundsReader
	// policyValidator refuses a dispatch that names a nonexistent HITL envelope at
	// CREATION time (see Dispatch). Nil skips the check — a fleetservice built
	// without WithPolicyValidator behaves as before — but serve and the in-process
	// editor both wire it, so a mission cannot be created against an envelope that
	// will never load and silently fall back to the default gate.
	policyValidator hitlservice.PolicyValidator
	// maxParallel is the fleet-width admission cap (see admission.go): how many
	// units may be open at once before Dispatch refuses. Defaults to
	// DefaultMaxParallel — enforced from birth, per the pando lesson — and 0 is
	// the explicit "unlimited" opt-out (WithMaxParallel / fleet-max-parallel=0).
	maxParallel int
	// admission serializes the count-then-allocate window of the cap check:
	// Dispatch holds it from admitUnit through StartResolved, so two concurrent
	// dispatches cannot both observe cap-1 open units and both allocate. It
	// guards nothing else — the rest of Dispatch runs unserialized as before.
	admission sync.Mutex
}

// Option configures a fleet Service at construction. It exists so the optional
// compute-bounds reader can be wired without changing New's positional signature
// for the callers that pass only the base dependencies (the missionservice.Option
// idiom).
type Option func(*service)

// WithPolicyValidator wires the creation-time envelope existence check Dispatch
// enforces (see service.policyValidator). Serve and the in-process editor pass a
// hitlservice.NewPolicyValidator over the same policy source their approval gate
// reads, so "a mission must name a real envelope" is enforced where missions are
// born, not discovered later as a silent fallback to the default policy.
func WithPolicyValidator(v hitlservice.PolicyValidator) Option {
	return func(s *service) { s.policyValidator = v }
}

// WithComputeBounds wires the reader the drive loop consults for a mission's
// envelope compute ceiling — the maxTurns and maxTokens bounds the HOST enforces
// between turns. hitlservice.Service satisfies it. Unset (the default) leaves those
// seams unbounded on every mission, so a fleetservice built without it behaves
// exactly as before compute bounds existed. (maxToolCalls does not flow through
// here: it is enforced per-call by the unattended answerer, which reads its bounds
// from the HITL service passed to NewUnattendedPermissionAnswerer.)
func WithComputeBounds(r hitlservice.ComputeBoundsReader) Option {
	return func(s *service) { s.computeBounds = r }
}

// New returns a Service driving instances (the kernel) and agents (for the
// Enabled policy check Dispatch enforces). workspaceRoots and projectRoot are
// dispatch-only and may be zero (an absent cwd then defaults to projectRoot
// unvalidated; see resolveCwd). missions is NOT optional: every dispatch is a
// mission (see DispatchRequest), so Dispatch calls into it unconditionally,
// with no per-request check standing in for a wiring problem. A nil registry
// here is the same class of defect as a nil instances or agents would be —
// the caller (contenox serve) always constructs a real one; it is not a
// condition Dispatch validates against a request. A nil tracker degrades to a
// Noop, so the async first-turn outcome is simply not recorded rather than
// panicking.
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
		// The width cap is enforced BEFORE any option runs: a fleetservice
		// nobody configured still refuses past DefaultMaxParallel, so the knob
		// cannot exist un-enforced (see admission.go). WithMaxParallel only
		// ever changes the value — including to 0 (unlimited).
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
	// Every dispatch is a mission (see DispatchRequest): the intent becomes
	// the unit's first turn and the HITL policy becomes its envelope, so
	// both are required up front rather than validated in combination with
	// each other or with whether a mission registry happens to be wired.
	if strings.TrimSpace(req.Intent) == "" {
		return DispatchResult{}, errdefs.MissingParameter("intent", "intent is required")
	}
	if strings.TrimSpace(req.HITLPolicyName) == "" {
		return DispatchResult{}, errdefs.MissingParameter("hitlPolicyName", "hitlPolicyName is required: a mission must name its envelope")
	}
	// The envelope is the load-bearing invariant of mission mode, so validate that
	// the named policy actually loads BEFORE anything is brought up. Without this a
	// typo (no-such-policy.json) is accepted and the mission runs to completion
	// under the silently-substituted default gate — the exact opposite of "you
	// author the approval gates". Validation is strict here at creation; the drive
	// loop's own bounds read stays fail-to-unbounded so a policy hiccup never kills
	// a mission that is already running.
	if s.policyValidator != nil {
		if err := s.policyValidator.ValidatePolicy(ctx, req.HITLPolicyName); err != nil {
			return DispatchResult{}, errdefs.InvalidParameterValue("hitlPolicyName",
				fmt.Sprintf("hitl policy %q could not be loaded: it must name an existing policy (see `contenox config` / the .contenox policy files)", strings.TrimSpace(req.HITLPolicyName)))
		}
	}
	// cwd envelope discipline: a requested cwd must be absolute and must resolve
	// within an allowlisted workspace root; an absent one defaults to the same
	// root the session path uses. The judgement is vfs.ResolveSessionCwd, shared
	// with acpsvc's session paths — see resolveCwd.
	cwd, err := s.resolveCwd(req.Cwd)
	if err != nil {
		return DispatchResult{}, err
	}

	// POLICY: refuse a disabled agent BEFORE bringing anything up, via the
	// ONE shared judgment agentregistryservice.ResolveForSpawn makes for
	// every agent-spawn path (see its doc comment) — this REST path and
	// acpsvc's external bring-up both call it, so the check cannot drift
	// between them. The kernel itself has no concept of Enabled (it spawns
	// whatever record it is handed).
	agent, err := agentregistryservice.ResolveForSpawn(ctx, s.agents, req.AgentName)
	if err != nil {
		if errors.Is(err, agentregistryservice.ErrAgentDisabled) {
			return DispatchResult{}, errdefs.Conflict(err.Error())
		}
		return DispatchResult{}, err
	}

	// The mission id is generated BEFORE the session is opened, because the unit
	// must hold it AT CONSTRUCTION: the mission tools a dispatched unit reports
	// through are bound into its session from this id (forwarded as session/new
	// `_meta`, missionservice.MissionMetaKey), not asserted by the agent
	// afterwards. Generating it here — rather than letting missionservice.Create
	// assign one — is what lets the same id ride into OpenSession below and be
	// persisted by Create afterwards. The mission ROW is still written after the
	// session opens (step 3), so nothing is persisted for a dispatch that fails
	// to bring a unit up; and the unit cannot file against the row before it
	// exists, because Dispatch sends its first Prompt (step 4) only after Create.
	missionID := uuid.NewString()

	// 1. Bring up an instance from the record the Enabled check was just made
	// against — but only past the fleet-width admission gate (admission.go):
	// count the units already open and refuse past the cap with a refusal that
	// teaches. Every dispatch passes this gate uniformly — fleetservice has no
	// separate operator relaunch/retry verb to carve a bypass for — and the
	// lock spans the check AND the allocation, so concurrent dispatches cannot
	// slip past the cap between counting and spawning.
	//
	// StartResolved, not Start(agentName): Start would re-read the same
	// row, which is both a second query per dispatch and a TOCTOU window — an
	// agent disabled between the two reads would still spawn, defeating the check
	// immediately above it.
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

	// 2. Open a session on the instance, handing the unit its mission id as
	// opaque session/new `_meta`. The kernel forwards the blob without
	// interpreting it; the unit's ACP agent parses it (missionservice.ParseMissionMeta)
	// and constructs the session with its mission tools bound to exactly this
	// mission. On failure tear the fresh instance down so a failed dispatch never
	// leaks a running subprocess (the acpsvc contract).
	sessionID, err := s.instances.OpenSession(ctx, instanceID, agentinstance.SessionSpec{
		Cwd:  cwd,
		Meta: missionservice.MarshalMissionMeta(missionID),
	})
	if err != nil {
		_ = s.instances.Stop(instanceID)
		return DispatchResult{}, err
	}

	result := DispatchResult{InstanceID: instanceID, SessionID: string(sessionID)}

	// 3. Record the mission — every dispatch is one — under the pre-generated id
	// the unit already holds, then bind both ids to it. The envelope
	// (HITLPolicyName) is set at creation, not bolted on after: a window between
	// "mission exists" and "mission has bounds" is exactly what mission mode must
	// not allow.
	m := &missionservice.Mission{
		ID:             missionID,
		Intent:         req.Intent,
		AgentName:      req.AgentName,
		HITLPolicyName: req.HITLPolicyName,
		// The supervision edge, recorded at creation from the only place that
		// knows it: whoever fired the dispatch. Empty when an operator fired it
		// directly — see DispatchRequest.ParentSessionID.
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

	// 4. The mission's turns run detached: Dispatch returns as soon as the session
	// is open and the mission is recorded; the outcome is observable on the board
	// and recorded through the tracker (never swallowed). context.WithoutCancel
	// keeps request-scoped values (request id) while surviving the caller's
	// return. Payload discipline holds — ids and stop reason to the tracker, never
	// prompt content — and the intent is stored CLEAN on the mission (the
	// unattended preamble is prepended on the wire only, never persisted).
	// The unit is allocated end to end: count it. Telemetry only (see
	// counters.go) — the admission gate above counts LIVE instances off the
	// kernel's board, never this ledger.
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

// missionRun is the fully-resolved input to the detached goroutine that shepherds
// a dispatched unit's turns. It carries IDS and the clean intent, not built
// prompt payloads: the goroutine assembles each turn's blocks itself (the
// preamble ahead of the intent, then the nudge) so the wire-only framing never
// leaks back into anything persisted.
type missionRun struct {
	instanceID string
	sessionID  libacp.SessionID
	missionID  string
	agentName  string
	intent     string
}

// driveUnattendedMission runs a dispatched unit's turns on the detached context
// and shepherds their OUTCOMES — the doctrine shift this package took on for
// mission mode (see the Service.Dispatch doc's amended "allocation, not
// operation"). It runs the intent as the first turn behind the unattended
// preamble, stamps liveness on every completed turn, and — if the unit produced
// no mission-tool fact — nudges exactly once before filing a blocker on the
// unit's behalf.
//
// The nudge loop is HARD-CAPPED at one. Two bare turns is enough evidence the
// unit will not report on its own; a runtime that kept nudging a mute unit would
// burn tokens narrating its own confusion and never converge. That cap is a
// decision, documented here so it is not "fixed" into an unbounded retry later:
// one nudge teaches, a second would nag, and past that the honest move is to hand
// the operator a blocker and stop talking.
func (s *service) driveUnattendedMission(ctx context.Context, run missionRun) {
	reportErr, reportChange, end := s.tracker.Start(ctx, "prompt", "fleet_dispatch",
		"instance_id", run.instanceID, "session_id", string(run.sessionID), "agent_name", run.agentName)
	defer end()

	// The mission's compute ceiling, read once at the start of the run. Best-effort
	// (see computeBoundsFor): an unwired reader or a policy-load hiccup yields the
	// zero, unbounded bounds, so the loop below is byte-for-byte its old self on
	// every mission whose envelope declares no compute block.
	bounds := s.computeBoundsFor(ctx, run.missionID)

	// Turn 1: the mission preamble (prevention) ahead of the CLEAN intent.
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

	// Turn 2 — the ONE bounded nudge — runs only if the envelope's turn budget
	// permits it. A tiny maxTurns stops the mission HERE (before a turn past its
	// budget is ever issued) and lands it stuck, rather than nudging: a mission out
	// of turns is not a mute unit to teach, it is a mission that spent its compute.
	if turnBudgetExceeded(2, bounds) {
		s.finishComputeStuck(ctx, run.missionID, turnsExhaustedReason(bounds.MaxTurns), reportChange)
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

	// Mute across BOTH turns: the runtime files the blocker itself. No third
	// prompt, ever. Routing to whoever supervises (a live parent session, or the
	// operator inbox when an operator fired directly) is the existing report
	// machinery's job, reached for free by AddReport publishing its event.
	s.fileSilentTurnBlocker(ctx, run)
}

// computeBoundsFor reads the mission's envelope compute ceiling for the drive-loop
// seams, or the zero (unbounded) bounds when no reader is wired or the read fails.
// Best-effort by design: a mission whose bounds cannot be read runs as it always
// did (unbounded), never blocked on a wiring gap or a policy-load hiccup — the same
// fail-to-unbounded stance ComputeBoundsFor itself takes on a load error.
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

// enforceTokenBudget stops a mission that has spent its token budget. It reads the
// unit's REPORTED usage from the session journal (best-effort — a unit whose
// provider emits no usage_update leaves maxTokens inert, documented on the
// envelope) and, when that reported usage crosses maxTokens, finishes the mission
// stuck and reports true so the drive loop stops. It never itself cancels a turn:
// it runs only between turns, once a turn has already completed.
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
	s.finishComputeStuck(ctx, run.missionID, tokensExhaustedReason(b.MaxTokens, used), reportChange)
	return true
}

// finishComputeStuck brings a mission to rest at StatusStuck through the REAL
// terminal machinery, with a reason naming the bound it crossed. Going through
// missionservice.Finish (not a bespoke write) is the whole point: the board, the
// operator inbox, and a `mission fire --wait` all read the terminal status and its
// reason and tell the truth for free. Best-effort on the write itself — a Finish
// that conflicts (the mission already terminal, e.g. the answerer finished it for
// maxToolCalls in the same turn) leaves the durable status correct anyway, so the
// error is recorded on the tracker and dropped rather than crashing the detached
// goroutine.
func (s *service) finishComputeStuck(ctx context.Context, missionID, reason string, reportChange func(string, any)) {
	reportChange("compute_bound", reason)
	if _, err := s.missions.Finish(ctx, missionID, missionservice.StatusStuck, reason); err != nil {
		reportChange("compute_bound_finish_error", err.Error())
	}
}

// sessionJournalReader is the OPTIONAL kernel capability the drive loop uses to
// read a mission session's reported token usage WITHOUT attaching a viewer (which
// would give the unattended session a controller and hijack its permission
// routing). The concrete agentinstance.Manager implements SessionJournal; it stays
// a narrow local interface reached by type assertion — the exact precedent
// sessionTextReader (SessionAgentText) sets for a policy-free journal read.
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
// outcome. Turn completion IS liveness — this is the fix for a mission whose
// "never reported" status meant nothing, because nothing ever stamped it: a clean
// turn clears LastError, a failed one records it so the failure is queryable off
// the board without attaching to anything. The heartbeat is best-effort (its
// error is deliberately dropped): a heartbeat write failing must neither swallow
// the turn's own error nor abort the run.
func (s *service) promptTurn(ctx context.Context, run missionRun, blocks []libacp.ContentBlock) (libacp.StopReason, error) {
	stop, err := s.instances.Prompt(ctx, run.instanceID, run.sessionID, blocks)
	if err != nil {
		_, _ = s.missions.Heartbeat(ctx, run.missionID, err.Error())
		return "", err
	}
	_, _ = s.missions.Heartbeat(ctx, run.missionID, "")
	return stop, nil
}

// missionReached reports whether the mission now carries any fact a unit produces
// through its mission tools — the cheapest honest answer to "did the unit's voice
// reach the operator this turn?", read straight off the durable mission store the
// unit writes to (no session attach, no transcript scrape). A fresh dispatch's
// mission starts with none of these, so any that appears between one turn and the
// next is the unit's own doing.
func (s *service) missionReached(ctx context.Context, missionID string) bool {
	m, err := s.missions.Get(ctx, missionID)
	if err != nil {
		// A mission we cannot read is not evidence the unit reached anyone; treat
		// it as not-reached rather than assuming success. The nudge/blocker that
		// follows is harmless if the mission is genuinely gone.
		m = nil
	}
	reportCount := 0
	if reports, rerr := s.missions.ListReports(ctx, missionID, 1); rerr == nil {
		reportCount = len(reports)
	}
	return missionShowsUnitReached(m, reportCount)
}

// missionShowsUnitReached is the pure decision at the heart of the nudge loop:
// given a mission and how many reports it carries, did the unit reach the operator
// through a mission tool? A filed report (mission_report, or the ask-attention
// fallback that records a blocker), a terminal verdict (mission_finish moved the
// mission off open), or a plan revision (mission_plan) each count — every one is a
// durable fact the unit itself wrote. A turn that left NONE of them is a turn that
// talked into the void.
//
// It deliberately does NOT try to see a durable attention ASK raised through a
// wired asker: that lands in the approval store, which this layer holds no handle
// on, and a unit that only asked for attention has already reached the operator.
// The worst case of not seeing it is one harmless extra nudge — the loop is capped
// at one regardless — so buying a dependency on the approval store to shave it
// would be the wrong trade.
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

// fileSilentTurnBlocker files the runtime's OWN blocker for a unit that ended two
// turns without reporting. It quotes the unit's last words when it can cheaply
// recover them from the kernel's session journal (an optional, policy-free read —
// see sessionTextReader), else falls back to a clear generic pointing at the
// session an operator can attach to. Best-effort: a failed AddReport leaves the
// mission showing no progress (still open, no report), which is the honest state
// anyway, and must not crash the detached goroutine.
func (s *service) fileSilentTurnBlocker(ctx context.Context, run missionRun) {
	summary, detail := silentTurnBlocker(s.lastAgentText(run.instanceID, run.sessionID), string(run.sessionID))
	_ = s.missions.AddReport(ctx, run.missionID, &missionservice.Report{
		Kind:    missionservice.ReportKindBlocker,
		Summary: summary,
		Detail:  detail,
	})
}

// sessionTextReader is the OPTIONAL capability fleetservice uses to quote a silent
// unit's own words: read the agent text the kernel already journals for a session,
// without attaching a viewer. The concrete agentinstance.Manager implements it
// (SessionAgentText); it stays a NARROW local interface reached by type assertion
// rather than a method on agentinstance.Manager, so the kernel's lifecycle
// interface need not grow a verb every mock must then implement, and a Manager
// that does not provide it simply yields no text (the generic blocker).
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

// The mission tools as the MODEL sees them: taskengine qualifies every tool with
// its provider name before offering it ("mission" + "." + "mission_report"), and
// the executor resolves the call by that same qualified name — which is why the
// unit's transcripts show `local_fs.read_file`, never `read_file`.
//
// These are DERIVED from the tool package's own constants, deliberately. They
// were once inlined as prose here, on the reasoning that a rename would be caught
// by the mission-tools tests — and the drift that actually happened was not a
// rename at all: the prose named the BARE tools (`mission_report`) while the model
// was offered the qualified ones, so an unattended unit was ordered, twice, to
// call a function that did not exist in its list. It answered in prose, got
// nudged, and returned an empty turn; the runtime then filed a blocker against a
// unit that had done the work and simply had no reachable way to say so. Deriving
// the strings makes that class of mismatch a compile-time impossibility.
var (
	toolAskAttention = missiontools.ToolsProviderName + "." + missiontools.ToolNameAskAttention
	toolReport       = missiontools.ToolsProviderName + "." + missiontools.ToolNameReport
	toolFinish       = missiontools.ToolsProviderName + "." + missiontools.ToolNameFinish
)

// missionPreamble is the unattended-context block Dispatch prepends to a unit's
// FIRST turn, ahead of the clean intent — prevention, before the void ever opens.
// It is WIRE-ONLY: never persisted as Mission.Intent, because it is context for
// the model, not the operator's stated goal. It is written as model-facing prompt
// surface — short, imperative, no fluff — and names the mission tools exactly as
// the model's tool list does (see the block above).
var missionPreamble = fmt.Sprintf(`You are running as an UNATTENDED mission unit. No human is reading this conversation — replying in prose reaches no one. You reach your operator ONLY through your mission tools:
- %s: ask a question, or flag a blocker you must not decide alone.
- %s: record real progress, a finding, or a result.
- %s: end the mission with a verdict, once the work is truly done.
Do the work with your other tools. When you need the operator, or have something worth their attention, call a mission tool. Chat text alone will not be seen.`, toolAskAttention, toolReport, toolFinish)

// missionNudge is the SINGLE follow-up turn a unit earns when its first turn
// produced no mission-tool fact: a terse reminder that it is unattended and must
// reach the operator through its mission tools, or keep working with its others.
// One nudge, ever (see driveUnattendedMission's hard cap).
var missionNudge = fmt.Sprintf(`Your last turn ended without reaching your operator, and no human is reading this chat. To reach your operator now, call %s (a question or a blocker) or %s (progress, a finding, or a result); to end the mission, call %s. If you are not done, keep working with your other tools and report when you have something. Do not answer in prose alone — it will not be seen.`, toolAskAttention, toolReport, toolFinish)

// silentTurnBlockerLead is the stable, greppable lead of the blocker the runtime
// files for a unit that ended two turns without reporting.
const silentTurnBlockerLead = "unit ended two turns without reporting"

// silentTurnBlocker builds the (single-line summary, multi-line detail) of the
// blocker the runtime files on a mute unit's behalf. When the unit's last words
// were cheaply recoverable, the summary carries a single-line EXCERPT of them (so
// the attention surface shows what it actually said) and the detail carries the
// full text plus where to attach; otherwise both fall back to a clear generic
// pointing at the session. The summary is always single-line and non-empty, which
// is exactly what missionservice.AddReport validation requires.
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

// singleLineExcerpt collapses all whitespace (newlines included) to single spaces
// — so the result is safe for a report Summary, which must be one line — and
// truncates to max runes with an ellipsis. An already-short, already-single-line
// string is returned essentially unchanged.
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
	// No session named: cancel every session currently attached on the
	// instance. Safe with no turn in flight (kernel contract), so an
	// instance with zero attached sessions is a no-op returning nil, not an
	// error.
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

// resolveCwd maps a requested session cwd onto the concrete root the dispatched
// unit will run in. It does NOT re-derive the rules: it delegates to
// vfs.ResolveSessionCwd — the same implementation the ACP session paths use — and
// owns only the translation of its refusal into this layer's REST error.
//
// The default for an ABSENT cwd is the EFFECTIVE workspace root, and the allowlist
// stays AUTHORITATIVE about what that is: with a workspace Factory configured (as
// `contenox serve` always does), an empty cwd resolves to the Factory's DEFAULT
// root — the allowlist's own first entry — and s.projectRoot is used only as the
// fallback when NO allowlist is configured (the unit-test wiring, the stdio path).
// So a stray or mis-wired projectRoot can never leak PAST a configured allowlist
// as a dispatched unit's cwd; TestFleetService_Dispatch_EmptyCwdResolvesToAllowlistDefault
// pins that.
//
// Traced footgun (why the first real dispatch landed a unit in $HOME): serve's
// effective workspace root defaults to filepath.Dir(~/.contenox) — i.e. $HOME —
// for an argument-less `contenox serve` (serve_cmd.go: workspaceRoot :=
// filepath.Dir(contenoxDir)), and that same root is passed BOTH as the Factory
// default and as this layer's projectRoot. An operator-fired mission with no cwd
// (`/mission <intent>` sets none) therefore resolves to $HOME — a permitted root
// there, but a poor place to drop a unit, since it is the parent of the
// control-plane dir. The RESOLUTION is correct: $HOME genuinely IS the configured
// default root for argument-less serve, so this seam must keep resolving an absent
// cwd to whatever the effective/default root actually is rather than inventing a
// different one. The remedy for the bad default is operational (serve a project
// directory, or configure a workspace-root), not a rewrite of this resolver — and
// keeping the allowlist authoritative here is what makes that remedy take effect.
//
// Note the tightening this inherits: a relative cwd is refused here, which the
// hand-rolled predecessor did not do. With no allowlist configured it passed a
// relative cwd straight through to OpenSession, where the ACP path would have
// refused it — POST /fleet/dispatch was the one door into session bring-up
// missing the absolute-path guard.
func (s *service) resolveCwd(cwd string) (string, error) {
	resolved, err := vfs.ResolveSessionCwd(s.workspaceRoots, cwd, s.projectRoot)
	if err != nil {
		return "", errdefs.InvalidParameterValue("cwd", err.Error())
	}
	return resolved, nil
}
