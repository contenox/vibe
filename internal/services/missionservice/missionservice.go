// Package missionservice stores mission records — the durable, agent-reportable
// half of the fleet manager (see
// docs/development/blueprints/acp/fleet-consolidation.md, "Mission mode"). A
// mission is not a note: it is the headless interaction model. An operator
// fires a one-line intent at a declared agent and detaches; the resulting
// unit runs unattended inside a permission envelope (a named HITL policy
// bound to the mission) and reports back — or asks for attention — through
// tools it holds only while on the mission. One mission binds exactly one
// session and one instance: the work a mission names is a single unit, so the
// record is too.
//
// Storage is runtimetypes KV records keyed by mission id under a shared prefix
// (pattern of acpsvc's acp:session_* keys, runtime/acpsvc/external.go), listed
// server-side via the store's prefix scan — zero migration. The validated-CRUD
// shape mirrors runtime/agentregistryservice so the two registries stay easy to
// compare.
//
// # Reports
//
// A mission's reports live under a second, sibling KV prefix keyed by mission
// id (missionReportKVPrefix; see that constant's comment for why it is a
// sibling of missionKVPrefix rather than nested under it). A Report's Kind is
// drawn from a small, closed set — progress, finding, blocker, result —
// rather than free text, and that closedness is a deliberate prompt-design
// choice: the set IS the hint to an unattended agent about what is worth
// reporting at all. A short, named enum tells the agent "these four shapes of
// thing are worth recording"; free text invites narration, which is the wrong
// incentive for a unit that should only cost a human's attention when it
// matters. Reports never carry artifact content — Refs points at it by path
// or URL only, keeping a report cheap to store and cheap to read from an
// attention inbox.
//
// # Liveness
//
// A mission runs unattended, so "is this unit still alive?" cannot be
// answered by a human watching a transcript — nobody is watching by
// construction. Without an explicit liveness signal, a mission whose unit
// crashed sits at StatusOpen forever, indistinguishable from one making slow,
// silent progress: exactly the failure mission mode must not have.
// LastHeartbeat and LastError turn liveness and last-failure into queryable
// facts instead of inferences: Heartbeat stamps LastHeartbeat and records (or
// clears) LastError on every call, so a caller can tell "working",
// "erroring", and "gone dark past some staleness threshold" apart without
// attaching to anything.
package missionservice

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/contenox/beam/internal/errdefs"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
)

// EventPublisher is the NARROW slice of the event bus AddReport uses to announce
// a new report. libbus.Messenger satisfies it. It is declared here, rather than
// importing libbus, so this package depends only on the one verb it calls and
// never on the whole bus — and so a missionservice built without a bus (every
// test today, the mission-only REST wiring) simply does not publish, with no
// nil-bus plumbing to thread through callers.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// ReportAddedSubject is the bus subject AddReport publishes a ReportAddedEvent
// on. It follows this codebase's "<package>.events[.<verb>]" bus-subject
// convention (see taskengine.TaskEventSubjectAll = "taskengine.events"). A
// routing service subscribes to it to deliver the report to the mission's
// supervisor — a live parent session, or the operator inbox when there is none.
//
// The seam is the point: missionservice PUBLISHES that a report exists and stays
// ignorant of sessions and inboxes; a separate service SUBSCRIBES and decides
// where it goes. That is the libbus idiom CONTRIBUTING.md names ("services
// publish typed events ... others subscribe without direct package coupling"),
// and it is what lets report routing work today off a REST-added report and
// compose automatically with the mission-tools slice when a unit files its own.
const ReportAddedSubject = "missionservice.events.report_added"

// ReportAddedEvent is the SELF-CONTAINED domain event AddReport publishes after
// a report is durably stored. Self-contained on purpose (the register of
// agentinstance.Event and UnattendedPermission): a subscriber routes it without
// reading anything back, so a routing service needs no missionservice handle to
// act on it.
//
// ParentSessionID is the supervision edge lifted onto the event from the
// mission: non-empty names the upstream session that fired the mission (route
// the report there), empty means an operator fired it directly (route it to the
// operator inbox). It is a fact ABOUT the mission, not a routing instruction —
// missionservice reports what happened; the subscriber decides what to do.
//
// The embedded Report carries the whole stored report, its OPTIONAL typed
// Handover included, so a subscriber that hands the report on to a next mission
// has the full hand-off in the event and never reads the report back — the
// self-contained-payload rule holds even as the report grew a structured half.
type ReportAddedEvent struct {
	MissionID       string `json:"missionId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	Report          Report `json:"report"`
}

// PlanRevisedSubject and StatusChangedSubject are the two sibling bus subjects
// this package publishes the plan engine's attention-worthy events on. They
// follow ReportAddedSubject's convention byte-for-byte (the "<package>.events.
// <verb>" form) and exist for the same reason: the inbox and board SUBSCRIBE to
// them the way reportrouter subscribes to ReportAddedSubject, and missionservice
// stays ignorant of who is listening. A plan revision and a terminal transition
// are exactly the "surfaced, never prevented" moments the blueprint's design law
// names (the inbox's "plan revised +2/−1 — <explanation>" line is a rendering of
// a PlanRevisedEvent), so they ride the bus rather than being polled for.
const (
	PlanRevisedSubject   = "missionservice.events.plan_revised"
	StatusChangedSubject = "missionservice.events.status_changed"
)

// PlanRevisedEvent is the SELF-CONTAINED event SetPlan publishes after a plan
// snapshot is durably stored. Self-contained in the register of ReportAddedEvent:
// a subscriber renders "plan revised" without a missionservice handle to read
// anything back.
//
// It carries both the revision's identity (Revision, Explanation) and the shape
// the inbox and board draw from: EntryCount plus the per-status counts for a
// progress bar, and Added/Removed — the "+2/−1" delta — computed by diffing the
// new snapshot's entry ids against the prior revision's. Added/Removed are a
// PRESENTATION number keyed on entry id (a fresh entry counts as added, an id
// that vanished from the snapshot counts as removed); they are honest for the
// single-writer planner that echoes ids and are never load-bearing for anything
// but the human-facing "what changed" line.
type PlanRevisedEvent struct {
	MissionID       string `json:"missionId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	Revision        int    `json:"revision"`
	Explanation     string `json:"explanation,omitempty"`
	EntryCount      int    `json:"entryCount"`
	Added           int    `json:"added"`
	Removed         int    `json:"removed"`
	Pending         int    `json:"pending"`
	InProgress      int    `json:"inProgress"`
	Completed       int    `json:"completed"`
}

// PlanRevisionSummary is one durable entry in a mission's bounded revision ring
// (Mission.PlanRevisions): the "+2/−1 — why" line for a single past SetPlan,
// kept so the overnight-skim inbox feed can show plan HISTORY, not only the
// current Plan.{Revision,Explanation}. It is the PlanRevisedEvent's presentation
// shape (Added/Removed delta plus the per-status counts) minus the routing edge
// (MissionID/ParentSessionID/AgentName/Intent, which the record already carries
// or the feed does not need), plus At — the wall-clock the revision was stored.
//
// WHY it is persisted on the record and not merely published: the event is a
// best-effort routing NUDGE that only fires when a bus is wired (see
// publishPlanRevised); the record is the durable fact. Decision (b) of the
// component roadmap (Tier 2 item 6) asks for a REAL history feed for the honest
// overnight-skim answer, which a fire-and-forget event cannot give — a subscriber
// that was down misses it forever. So the summary write rides INSIDE the durable
// SetPlan put, next to the plan snapshot itself: it is present whether or not the
// bus is, and a mission that ran with no publisher still accrues its full history.
//
// The `at` field is the same wall-clock stamped on Mission.UpdatedAt for the same
// revision, so a reader can order the ring chronologically without a second clock.
type PlanRevisionSummary struct {
	Revision    int       `json:"revision"`
	Explanation string    `json:"explanation,omitempty"`
	Added       int       `json:"added"`
	Removed     int       `json:"removed"`
	Pending     int       `json:"pending"`
	InProgress  int       `json:"inProgress"`
	Completed   int       `json:"completed"`
	At          time.Time `json:"at"`
}

// StatusChangedEvent is the SELF-CONTAINED event Finish publishes after a
// mission comes to rest in a terminal state. OldStatus/NewStatus name the
// transition (always open-or-running → terminal, since Finish rejects every
// other move) and Reason carries the one line the caller gave for why — the same
// string persisted as Mission.StatusReason. Same register as ReportAddedEvent:
// the inbox routes and renders it without reading the mission back.
type StatusChangedEvent struct {
	MissionID       string `json:"missionId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	OldStatus       Status `json:"oldStatus"`
	NewStatus       Status `json:"newStatus"`
	Reason          string `json:"reason,omitempty"`
}

// Status is a mission's lifecycle state: one running state (open) and a closed
// set of TERMINAL states a finished mission comes to rest in. The terminal set
// is the mission-plan blueprint's "hard facts a planner reasons over" — a landed
// mission handed real context to the next one; a derailed one failed; a stuck
// one hit a discrete boundary the caller must judge (see the register on
// StatusStuck). abandoned predates the blueprint and stays in the set: it is the
// operator's "I gave up on this" label, distinct from the three the RUNTIME (or
// a unit, once it can) records on its own.
//
// The values are contracted, not incidental: existing KV rows carry these exact
// strings, so the set only ever GROWS (a new terminal state is added, never a
// rename), and old rows keep parsing unchanged.
type Status string

const (
	StatusOpen      Status = "open"
	StatusLanded    Status = "landed"
	StatusDerailed  Status = "derailed"
	StatusAbandoned Status = "abandoned"

	// StatusStuck is a FIRST-CLASS terminal signal, deliberately distinct from
	// StatusDerailed rather than folded into it. The blueprint treats
	// "stuck" as a discrete boundary a mission can
	// hit — a loop, a wall it cannot get past, a judgement it cannot make alone —
	// and it is worth a different word than "failed" because it asks for a
	// different response (attention, a nudge, a replan) than a derailment does
	// (a post-mortem). DETECTING stuck is the caller's business — a heuristic, a
	// planner's judgement, an operator's call — never this layer's; the runtime
	// only owns the discrete STATUS, exactly as it owns the closed report-kind
	// set without owning what counts as progress.
	StatusStuck Status = "stuck"
)

// isTerminalStatus reports whether a mission in this state is FINISHED — at rest
// in one of the closed terminal states, never to move again through the guarded
// Finish path. It backs both halves of Finish's guard: the target must be
// terminal, and a mission already terminal is immutable. abandoned is included
// because it is an end-state too, so an operator's abandon cannot later be
// overwritten by a unit's Finish (the operator's manual PATCH/Update remains the
// one deliberately-unguarded override — see Finish).
func isTerminalStatus(status Status) bool {
	switch status {
	case StatusLanded, StatusDerailed, StatusStuck, StatusAbandoned:
		return true
	default:
		return false
	}
}

// missionKVPrefix namespaces mission records in the KV store; each mission is
// stored at missionKVPrefix+ID and the set is listed by this prefix.
const missionKVPrefix = "fleet:mission:"

// missionReportKVPrefix namespaces mission reports; each report is stored at
// missionReportKVPrefix+missionID+":"+reportID, and a mission's reports are
// listed by scanning the prefix missionReportKVPrefix+missionID+":".
//
// It is deliberately a SIBLING of missionKVPrefix, not a child of it (i.e.
// NOT "fleet:mission:report:") — missionKVPrefix ("fleet:mission:") is used
// as a raw LIKE prefix by List() to scan every mission, and nesting reports
// under it would make every report key match that same scan, corrupting the
// mission list with report rows decoded as missions. "fleet:mission_report:"
// does not share "fleet:mission:" as a string prefix (the byte after
// "fleet:mission" differs: '_' vs ':'), so the two prefix scans can never
// collide.
const missionReportKVPrefix = "fleet:mission_report:"

// Mission is the headless interaction model's durable record: a one-line
// intent fired at a declared agent, bound to exactly one session and one
// instance, and bounded by an envelope — a named HITL policy — that governs
// what the unit may do while unattended. It may outlive its session and
// instance and remains listed while open.
//
// LastHeartbeat and LastError are liveness facts, not status: a mission stays
// StatusOpen for its whole run, and these two fields are how a caller tells a
// unit that is quietly working apart from one that has gone dark or is
// erroring, without attaching to its session. Neither is required on create;
// both start zero (LastHeartbeat nil, LastError "") until Heartbeat is
// called.
//
// ParentSessionID is the SUPERVISION EDGE: the upstream session that FIRED this
// mission, which is not the same thing as the session it spawned.
// SessionID/InstanceID name the unit the mission created; ParentSessionID names
// who created it. A mission fired from a chat session (the `/mission` slash
// command) is supervised by THAT session — the fired unit's reports belong to
// the caller who can act on them, not to an operator inbox nobody is reading. It
// is empty when an operator fired the mission directly, which is also the "route
// reports to the operator inbox" case. This layer only RECORDS the edge;
// runtime/reportrouter consumes it (delivering reports to the parent session or
// the operator inbox).
//
// It is the same missing edge C2 surfaced from the other side (an approval row
// that cannot name who is asking): one absent relationship, two symptoms.
//
// Plan is the mission's living plan (see Plan and SetPlan) — the reviewable
// record a resident planner owns, never a schedule the runtime executes. A
// mission that was never planned carries the zero Plan (revision 0, no entries),
// which is also exactly what a legacy row written before this field existed
// decodes to. PlanRevisions is the bounded ring of past revision summaries (see
// PlanRevisionSummary and SetPlan) — the durable "+2/−1 — why" history the inbox
// feed skims; it is additive and omitempty, so a legacy row (or a mission never
// planned) decodes to a nil ring exactly as before. StatusReason is the one line Finish attaches to a terminal
// transition — WHY a mission derailed or got stuck — kept as a durable fact so
// the reason survives the fire-and-forget status_changed event that also carries
// it. It is empty for a running mission and (by the register below) for one that
// landed cleanly.
type Mission struct {
	ID              string `json:"id"`
	Intent          string `json:"intent"`
	AgentName       string `json:"agentName"`
	HITLPolicyName  string `json:"hitlPolicyName"`
	SessionID       string `json:"sessionId,omitempty"`
	InstanceID      string `json:"instanceId,omitempty"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	Status          Status `json:"status"`
	StatusReason    string `json:"statusReason,omitempty"`
	Plan            Plan   `json:"plan"`
	// PlanRevisions is the last-N revision summaries, oldest-first (newest is the
	// final element). Bounded by maxPlanRevisions; nil on a never-planned or legacy
	// mission. Surfaced additively on the mission GET as `planRevisions`.
	PlanRevisions []PlanRevisionSummary `json:"planRevisions,omitempty"`
	LastHeartbeat *time.Time            `json:"lastHeartbeat,omitempty"`
	LastError     string                `json:"lastError,omitempty"`
	CreatedAt     time.Time             `json:"createdAt"`
	UpdatedAt     time.Time             `json:"updatedAt"`
}

// ReportKind is the closed, small set of things a mission report may be. See
// the package doc's "Reports" section for why the set is deliberately closed
// rather than free text.
type ReportKind string

const (
	ReportKindProgress ReportKind = "progress"
	ReportKindFinding  ReportKind = "finding"
	ReportKindBlocker  ReportKind = "blocker"
	ReportKindResult   ReportKind = "result"
)

// Report is a single dispatch from a unit on a mission, filed under a Kind
// that hints how much it matters. Refs is by reference only (file paths,
// URLs, etc.) — a report never carries artifact content.
//
// Handover is the OPTIONAL typed hand-off (see Handover). It is a pointer so
// its ABSENCE is a first-class, wire-visible fact: a nil Handover is a legacy
// report (or any report the unit chose not to attach a hand-off to), and such a
// report round-trips through JSON exactly as it did before this field existed —
// the field is purely additive and every pre-handover row decodes to nil. It is
// meaningful mostly on a `result` report that a NEXT mission will build on; the
// Kind set stays unchanged, because the hand-off is extra STRUCTURE on a report,
// not a new KIND of report.
type Report struct {
	ID        string     `json:"id"`
	MissionID string     `json:"missionId"`
	Kind      ReportKind `json:"kind"`
	Summary   string     `json:"summary"`
	Detail    string     `json:"detail,omitempty"`
	Refs      []string   `json:"refs,omitempty"`
	Handover  *Handover  `json:"handover,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// Handover is the structured hand-off a mission attaches to a report so the NEXT
// mission builds on real context instead of prose (the blueprint's ontology: a
// landed mission "hands real context to the next one"; the old summarizer's typed
// `{outcome, artifacts, handover_for_next, caveats}` shape adapted to a report —
// Summary already lives on the Report, so the hand-off carries the rest). Every
// field is optional: a report may fill all, some, or none, and a report with an
// empty Handover is indistinguishable from one with none (both round-trip as a
// nil pointer once the empties are dropped — see AddReport's normalization).
//
//   - Outcome: the one-line verdict of what this mission actually achieved, the
//     hand-off's headline ("ported the hot loop; benchmarks pending").
//   - Artifacts: the concrete deliverables this mission produced that the next
//     one consumes — paths or URLs, by reference only, never inline content (the
//     same by-reference discipline Report.Refs holds). Distinct from Refs, which
//     are supporting pointers for THIS report; Artifacts are what the next mission
//     is handed to work FROM.
//   - HandoverForNext: the free-text brief to the next mission — what to pick up,
//     what is already done, what to watch for. The substance the model owns.
//   - Caveats: the known limitations, unverified assumptions, or risks the next
//     mission must not take for granted — the honest small print on the hand-off.
type Handover struct {
	Outcome         string   `json:"outcome,omitempty"`
	Artifacts       []string `json:"artifacts,omitempty"`
	HandoverForNext string   `json:"handoverForNext,omitempty"`
	Caveats         string   `json:"caveats,omitempty"`
}

// IsEmpty reports whether a hand-off carries no substance — every field blank.
// AddReport uses it to drop an all-empty Handover to nil, so "a hand-off with
// nothing in it" and "no hand-off" are the same durable fact rather than two
// shapes a reader must tell apart.
func (h *Handover) IsEmpty() bool {
	if h == nil {
		return true
	}
	if strings.TrimSpace(h.Outcome) != "" ||
		strings.TrimSpace(h.HandoverForNext) != "" ||
		strings.TrimSpace(h.Caveats) != "" {
		return false
	}
	for _, a := range h.Artifacts {
		if strings.TrimSpace(a) != "" {
			return false
		}
	}
	return true
}

// PlanEntryStatus is a plan entry's lifecycle state. Its VALUES are contracted
// to be byte-for-byte libacp.PlanEntryStatus (pending|in_progress|completed):
// the plan record is projected to ACP as full-snapshot plan updates (the next
// slice), and value-parity turns that projection into a cast rather than a
// translation table nobody remembers to keep in sync. The type is kept DISTINCT
// on purpose — missionservice owns a durable record and must not import a
// transport package to describe it (the same reason ReportKind is local rather
// than borrowed) — but the strings are a promise the projection slice leans on,
// and a conformance test there pins them.
type PlanEntryStatus string

const (
	PlanEntryPending    PlanEntryStatus = "pending"
	PlanEntryInProgress PlanEntryStatus = "in_progress"
	PlanEntryCompleted  PlanEntryStatus = "completed"
)

// PlanEntryPriority mirrors libacp.PlanEntryPriority the same way, and for the
// same reason.
type PlanEntryPriority string

const (
	PlanEntryPriorityHigh   PlanEntryPriority = "high"
	PlanEntryPriorityMedium PlanEntryPriority = "medium"
	PlanEntryPriorityLow    PlanEntryPriority = "low"
)

// PlanEntry is one line of a mission's plan: a short piece of work with a
// status and a priority. ID is the entry's stable identity ACROSS revisions —
// it is what lets a full-snapshot replace (see SetPlan) be diffed against the
// prior snapshot without matching on content, and it is what the completed-work
// immutability guard keys on. SetPlan assigns an id to any entry that arrives
// without one, so a planner may carry an entry forward by echoing its id and
// introduce a new one simply by omitting it.
type PlanEntry struct {
	ID       string            `json:"id"`
	Content  string            `json:"content"`
	Status   PlanEntryStatus   `json:"status"`
	Priority PlanEntryPriority `json:"priority"`
}

// Plan is a mission's LIVING plan — an ordered list of entries owned by exactly
// one planner, held here as the reviewable record (blueprint's design law: the
// runtime stores, validates, and publishes the plan; it never compiles it into
// control flow). It is never a schedule and nothing here reads it back to decide
// what runs next.
//
// Revision counts successful SetPlan calls: 0 is "never planned" (the zero Plan
// every pre-plan mission and every legacy KV row decodes to), 1 is the first
// snapshot, and it climbs by one per replace. Explanation is the LATEST
// revision's rationale (blueprint pattern 1, Codex's explanation-per-revision) —
// the one line the "plan revised" inbox entry carries; only the current
// revision's is retained, since the record keeps only the current snapshot.
type Plan struct {
	Entries     []PlanEntry `json:"entries"`
	Revision    int         `json:"revision"`
	Explanation string      `json:"explanation,omitempty"`
}

// Service exposes validated CRUD over mission records, Bind (which attaches
// this mission's one session and one instance), Heartbeat (unattended
// liveness), and mission reports.
type Service interface {
	Create(ctx context.Context, m *Mission) error
	Get(ctx context.Context, id string) (*Mission, error)

	// GetByInstance returns the mission bound to instanceID, or
	// libdb.ErrNotFound when no mission claims it (a fleet unit brought up
	// outside a dispatch — an ACP chat session's external agent, for
	// instance — has none, and that is a normal answer, not a failure).
	//
	// It is the lookup the unattended-permission path needs: the kernel knows
	// only which INSTANCE raised a request, while the envelope that governs it
	// lives on the MISSION. See the implementation for why this scans rather
	// than maintaining a secondary index, and what it does with a duplicate
	// claim.
	GetByInstance(ctx context.Context, instanceID string) (*Mission, error)

	List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Mission, error)
	Update(ctx context.Context, m *Mission) error
	Delete(ctx context.Context, id string) error

	// Bind attaches sessionID and/or instanceID to mission id — the
	// one-mission, one-unit invariant mission mode requires. Re-binding the
	// id a mission already carries is an idempotent no-op; binding a
	// DIFFERENT id over one already set is a conflict
	// (errdefs.ErrConflict) rather than an append. An unknown mission
	// id surfaces as libdb.ErrNotFound.
	Bind(ctx context.Context, id string, sessionID, instanceID string) (*Mission, error)

	// Heartbeat records that mission id's unit is still alive: it stamps
	// LastHeartbeat to now and sets LastError to lastErr (empty clears any
	// previously recorded error), then bumps UpdatedAt and persists. An
	// unknown mission id surfaces as libdb.ErrNotFound. Nothing calls this
	// yet — the caller arrives with the mission-tools slice.
	Heartbeat(ctx context.Context, id string, lastErr string) (*Mission, error)

	// Finish moves mission id into a terminal state (landed | derailed | stuck
	// | abandoned), records reason as the durable StatusReason, and publishes a
	// StatusChangedEvent. It is GUARDED — the one transition mission mode makes
	// hard truth: only a non-terminal mission may be finished, a terminal status
	// is the only permitted target, and a mission already terminal is immutable.
	// A second Finish naming the SAME terminal status is an idempotent no-op
	// (safe to retry); a DIFFERENT terminal status over an already-finished
	// mission is errdefs.ErrConflict (a landed mission does not later
	// become derailed). An unknown mission id surfaces as libdb.ErrNotFound.
	Finish(ctx context.Context, id string, status Status, reason string) (*Mission, error)

	// SetPlan replaces mission id's plan with a FULL SNAPSHOT (blueprint pattern
	// 4: each call replaces the whole list, deletion is omission from the
	// snapshot), bumps the revision counter, records explanation as the new
	// revision's rationale, and publishes a PlanRevisedEvent. Entries are
	// validated for SHAPE (count/size caps, empty/garbage rejection, known
	// status/priority) but NOT for planning DISCIPLINE (no "exactly one
	// in_progress" rule — that is the planner prompt's job, blueprint pattern 3).
	// The one audit-safety guard it does enforce: a snapshot may not rewrite the
	// content of an entry that was already completed in the prior revision
	// (matched by id) — completed work is immutable, corrections are appended as
	// new entries (blueprint pattern 5). An unknown mission id surfaces as
	// libdb.ErrNotFound.
	SetPlan(ctx context.Context, id string, entries []PlanEntry, explanation string) (*Mission, error)

	// AddReport validates report (Kind, Summary), assigns an id and
	// CreatedAt when absent, binds it to missionID, and persists it.
	// missionID must name an existing mission — an unknown one surfaces as
	// libdb.ErrNotFound rather than a silent insert.
	AddReport(ctx context.Context, missionID string, report *Report) error

	// ListReports returns missionID's reports newest-first. The slice is
	// always non-nil, so a mission with no reports yet renders as [].
	ListReports(ctx context.Context, missionID string, limit int) ([]*Report, error)
}

type service struct {
	db  libdb.DBManager
	pub EventPublisher
}

// Option configures a mission service at construction. It exists so the one
// optional dependency this service has (the event publisher) can be wired
// without changing New's signature for the many callers that pass only a db.
type Option func(*service)

// WithEventPublisher wires the bus AddReport publishes ReportAddedEvent on. When
// unset (the default) AddReport stores the report and publishes nothing — the
// report is still the durable fact; only its live routing nudge is skipped — so
// a missionservice built without a bus behaves exactly as before this seam
// existed.
func WithEventPublisher(pub EventPublisher) Option {
	return func(s *service) { s.pub = pub }
}

// New creates a mission service backed by the given database manager.
func New(db libdb.DBManager, opts ...Option) Service {
	s := &service{db: db}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *service) store() runtimetypes.Store {
	return runtimetypes.New(s.db.WithoutTransaction())
}

// Create validates m (intent, envelope, status), assigns an id when absent,
// forces the status to open, stamps timestamps, and persists it. A mission
// with no HITLPolicyName is rejected: the envelope is what bounds an
// unattended unit, so a mission without one is a mission with no bounds,
// which mission mode must not permit.
func (s *service) Create(ctx context.Context, m *Mission) error {
	if m == nil {
		return fmt.Errorf("mission is required")
	}
	m.Status = StatusOpen
	if err := validate(m); err != nil {
		return err
	}
	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	return s.put(ctx, m, false)
}

func (s *service) Get(ctx context.Context, id string) (*Mission, error) {
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	var m Mission
	if err := s.store().GetKV(ctx, missionKVPrefix+id, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// scanPageSize bounds one page of the mission prefix scan GetByInstance walks.
// Missions are small records and the scan is off the hot path (once per
// unattended permission request, not per token), so the page exists to keep one
// query's result set bounded rather than to make the walk fast.
const scanPageSize = 200

// GetByInstance implements Service.GetByInstance by SCANNING the mission
// records, newest-first, and returning the first whose InstanceID matches.
//
// # Why a scan and not a secondary key
//
// The alternative is a second KV entry (instance id -> mission id) written at
// Bind time. It would be O(1) instead of O(n), and it would be a SECOND source
// of truth for a fact the mission record already owns — one that can be written
// and then not written (a Bind that half-succeeds), can outlive the mission it
// points at, and would need its own repair path when the two disagree. That is
// the shape of defect this subsystem already produced once (an index consumed
// by a second slice before anything used it end to end), and the blueprint's
// standing invariant is "no second mechanism".
//
// The cost is bounded by what n actually is: missions are dispatches, one per
// unit of work an operator or an agent fired, on one workstation. A scan of a
// few hundred small JSON records, performed once per unattended permission
// request, is not a cost worth buying an index-consistency problem to avoid. If
// n ever grows past that, the fix is a real indexed column on a real table, not
// a hand-maintained KV pointer.
//
// # Two missions claiming one instance
//
// Bind refuses to move a mission from one instance to another, so a mission's
// claim never changes once made — but nothing makes the claim EXCLUSIVE across
// missions, and Dispatch creates a fresh instance per mission, so a duplicate
// can only arise from a hand-written Bind against an already-claimed unit. The
// scan resolves it deterministically rather than pretending it cannot happen:
// the prefix scan is newest-first and the FIRST match wins, i.e. the most
// recently created mission that claims the unit. That is the answer that
// matches what a duplicate claim means in practice ("this unit was re-purposed
// for newer work"), and it is stable — the same call returns the same mission
// every time, so an ask's envelope cannot flip between two evaluations.
func (s *service) GetByInstance(ctx context.Context, instanceID string) (*Mission, error) {
	if instanceID == "" {
		return nil, fmt.Errorf("instanceId is required")
	}
	var cursor *time.Time
	for {
		batch, next, err := s.listPage(ctx, cursor, scanPageSize)
		if err != nil {
			return nil, err
		}
		for _, m := range batch {
			if m.InstanceID == instanceID {
				return m, nil
			}
		}
		if len(batch) < scanPageSize || next == nil {
			return nil, libdb.ErrNotFound
		}
		// The strictly-decreasing-cursor guard defends against an
		// identical-timestamp storm looping forever, at the cost of truncating
		// such a tie — the same limitation every other prefix-scan pager in this
		// codebase carries.
		if cursor != nil && !next.Before(*cursor) {
			return nil, libdb.ErrNotFound
		}
		cursor = next
	}
}

// List returns missions newest-first via the store's prefix scan. The slice is
// always non-nil so an empty fleet renders as [].
func (s *service) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Mission, error) {
	if limit <= 0 {
		limit = 100
	}
	missions, _, err := s.listPage(ctx, createdAtCursor, limit)
	return missions, err
}

// listPage is List's implementation plus the cursor for the NEXT page: the
// STORE-side created_at of the oldest row it returned. Paging on that rather
// than on Mission.CreatedAt matters — the two are close but not equal (the
// mission stamps its own CreatedAt just before the row is written), and feeding
// the record's timestamp back into a scan ordered by the row's would silently
// skip every mission written in the gap between them.
func (s *service) listPage(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Mission, *time.Time, error) {
	kvs, err := s.store().ListKVPrefix(ctx, missionKVPrefix, createdAtCursor, limit)
	if err != nil {
		return nil, nil, err
	}
	missions := make([]*Mission, 0, len(kvs))
	for _, kv := range kvs {
		var m Mission
		if err := json.Unmarshal(kv.Value, &m); err != nil {
			return nil, nil, fmt.Errorf("mission %q: %w", kv.Key, err)
		}
		missions = append(missions, &m)
	}
	var next *time.Time
	if len(kvs) > 0 {
		last := kvs[len(kvs)-1].CreatedAt
		next = &last
	}
	return missions, next, nil
}

// Update validates m and persists intent/status/envelope/reference changes to
// an existing mission. An unknown id surfaces as libdb.ErrNotFound. The caller
// owns m's CreatedAt (typically read via Get); UpdatedAt is restamped here.
func (s *service) Update(ctx context.Context, m *Mission) error {
	if m == nil {
		return fmt.Errorf("mission is required")
	}
	if m.ID == "" {
		return fmt.Errorf("id is required for update")
	}
	if err := validate(m); err != nil {
		return err
	}
	m.UpdatedAt = time.Now().UTC()
	return s.put(ctx, m, true)
}

func (s *service) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	return s.store().DeleteKV(ctx, missionKVPrefix+id)
}

// Bind attaches sessionID and/or instanceID to mission id. Mission mode binds
// exactly one session and one instance per mission, so binding is not
// additive: setting an id the mission does not yet carry succeeds,
// re-setting the id it already carries is an idempotent no-op, and setting a
// DIFFERENT id over one already bound is a conflict — a mission does not
// switch which unit it names mid-flight; a caller that wants a different unit
// dispatches a new mission instead of rebinding this one. An unknown mission
// id surfaces as libdb.ErrNotFound.
func (s *service) Bind(ctx context.Context, id string, sessionID, instanceID string) (*Mission, error) {
	if sessionID == "" && instanceID == "" {
		return nil, fmt.Errorf("bind requires a sessionId or instanceId")
	}
	m, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	changed := false
	if sessionID != "" {
		switch m.SessionID {
		case "":
			m.SessionID = sessionID
			changed = true
		case sessionID:
			// Idempotent re-bind of the id already carried: no-op.
		default:
			return nil, errdefs.Conflict(fmt.Sprintf("mission %q is already bound to session %q", id, m.SessionID))
		}
	}
	if instanceID != "" {
		switch m.InstanceID {
		case "":
			m.InstanceID = instanceID
			changed = true
		case instanceID:
			// Idempotent re-bind of the id already carried: no-op.
		default:
			return nil, errdefs.Conflict(fmt.Sprintf("mission %q is already bound to instance %q", id, m.InstanceID))
		}
	}
	if !changed {
		return m, nil
	}

	m.UpdatedAt = time.Now().UTC()
	if err := s.put(ctx, m, true); err != nil {
		return nil, err
	}
	return m, nil
}

// Heartbeat records that mission id's unit is still alive: it stamps
// LastHeartbeat to now, sets LastError to lastErr (empty clears it), bumps
// UpdatedAt, and persists. See the package doc's "Liveness" section for why
// this has to be an explicit, agent-reported fact rather than something a
// human infers. An unknown mission id surfaces as libdb.ErrNotFound.
func (s *service) Heartbeat(ctx context.Context, id string, lastErr string) (*Mission, error) {
	m, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	m.LastHeartbeat = &now
	m.LastError = lastErr
	m.UpdatedAt = now
	if err := s.put(ctx, m, true); err != nil {
		return nil, err
	}
	return m, nil
}

// put marshals m and writes it to the KV store. When mustExist is true it uses
// UpdateKV, whose zero-rows-affected result surfaces as libdb.ErrNotFound so an
// update to a missing mission is a not-found rather than a silent insert.
func (s *service) put(ctx context.Context, m *Mission, mustExist bool) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal mission: %w", err)
	}
	if mustExist {
		return s.store().UpdateKV(ctx, missionKVPrefix+m.ID, raw)
	}
	return s.store().SetKV(ctx, missionKVPrefix+m.ID, raw)
}

// AddReport validates report (Kind, Summary), assigns an id and CreatedAt
// when absent, binds it to missionID — overriding whatever MissionID the
// caller supplied, since the argument is authoritative — and persists it.
// missionID is checked against the mission store FIRST, so posting a report
// against an unknown mission surfaces as libdb.ErrNotFound rather than a
// silent insert (the report KV namespace carries no foreign key, so nothing
// else would catch this).
func (s *service) AddReport(ctx context.Context, missionID string, report *Report) error {
	if missionID == "" {
		return fmt.Errorf("missionId is required")
	}
	if report == nil {
		return fmt.Errorf("report is required")
	}
	// Fetch (not just existence-check) the mission: it both proves the mission
	// exists — the not-found guard — and hands us the supervision edge the
	// ReportAddedEvent carries, with no second read.
	m, err := s.Get(ctx, missionID)
	if err != nil {
		return err
	}
	report.MissionID = missionID
	// Collapse an all-empty hand-off to nil BEFORE validation and storage, so a
	// report the unit tagged with a blank Handover is stored as a legacy report
	// rather than an empty-object shape a reader must special-case (see Handover).
	if report.Handover.IsEmpty() {
		report.Handover = nil
	}
	if err := validateReport(report); err != nil {
		return err
	}
	if report.ID == "" {
		report.ID = uuid.NewString()
	}
	report.CreatedAt = time.Now().UTC()

	raw, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	// Persist the durable fact FIRST, then nudge (the ADOPTED enqueue-then-nudge
	// order of fleet-consolidation.md): the report is stored before anyone is
	// told it exists, so a lost or late nudge never loses the report.
	if err := s.store().SetKV(ctx, missionReportKVPrefix+missionID+":"+report.ID, raw); err != nil {
		return err
	}
	s.publishReportAdded(ctx, m, report)
	return nil
}

// publishReportAdded announces a stored report on ReportAddedSubject. It is
// BEST EFFORT and never surfaces to AddReport's caller: the report is already
// the durable fact (persisted above) and remains readable via ListReports and
// the operator inbox regardless, so a publish failure must not turn a
// successfully-recorded report into a failed AddReport. Routing is best-effort
// delivery layered on top of a durable record — that is the invariant, and this
// is where it is enforced. A no-op when no publisher was wired.
func (s *service) publishReportAdded(ctx context.Context, m *Mission, report *Report) {
	if s.pub == nil {
		return
	}
	ev := ReportAddedEvent{
		MissionID:       m.ID,
		ParentSessionID: m.ParentSessionID,
		AgentName:       m.AgentName,
		Intent:          m.Intent,
		Report:          *report,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		slog.Warn("missionservice: marshal report-added event failed; report stored, routing nudge skipped",
			"missionId", m.ID, "reportId", report.ID, "error", err)
		return
	}
	if err := s.pub.Publish(ctx, ReportAddedSubject, data); err != nil {
		slog.Warn("missionservice: publish report-added event failed; report stored, routing nudge skipped",
			"missionId", m.ID, "reportId", report.ID, "error", err)
	}
}

// ListReports returns missionID's reports newest-first via the store's prefix
// scan. The slice is always non-nil so a mission with no reports yet (or an
// unknown missionID) renders as [].
func (s *service) ListReports(ctx context.Context, missionID string, limit int) ([]*Report, error) {
	if missionID == "" {
		return nil, fmt.Errorf("missionId is required")
	}
	if limit <= 0 {
		limit = 100
	}
	kvs, err := s.store().ListKVPrefix(ctx, missionReportKVPrefix+missionID+":", nil, limit)
	if err != nil {
		return nil, err
	}
	reports := make([]*Report, 0, len(kvs))
	for _, kv := range kvs {
		var rep Report
		if err := json.Unmarshal(kv.Value, &rep); err != nil {
			return nil, fmt.Errorf("report %q: %w", kv.Key, err)
		}
		reports = append(reports, &rep)
	}
	return reports, nil
}

// Finish implements Service.Finish. See the interface doc for the guard; this is
// where it lives.
//
// # Why the guard is here and not in Update
//
// Update is the low-level, unguarded write — the operator's manual PATCH goes
// through it, and an operator correcting a mislabeled mission (even un-finishing
// one) is a legitimate act, so Update stays able to set any valid status. Finish
// is the AGENT-REPORTABLE, hard-fact path: a unit (or the runtime on its behalf)
// declaring "this work is over, and here is why". That path is the one that must
// not let a finished mission be silently re-terminalized, because the terminal
// status is what downstream planning treats as settled truth. Keeping the guard
// on Finish and off Update is a decision, not an oversight: it puts immutability
// exactly where the audit story needs it (the automated writer) and leaves the
// human override deliberately unguarded (the manual one).
//
// Idempotent-same-status is the other deliberate call: a retried Finish that
// names the state the mission is already in returns it unchanged rather than
// erroring, so a caller that lost the response to a network blip can safely
// repeat the call; a Finish naming a DIFFERENT terminal state is the conflict,
// because that is a genuine contradiction, not a retry.
func (s *service) Finish(ctx context.Context, id string, status Status, reason string) (*Mission, error) {
	if !isTerminalStatus(status) {
		return nil, fmt.Errorf("cannot finish mission %q as %q: a terminal status is required (landed|derailed|stuck|abandoned)", id, status)
	}
	m, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if isTerminalStatus(m.Status) {
		if m.Status == status {
			// Idempotent no-op: a retry of the same terminal transition. Return
			// the record untouched — no restamp, no second event — so repetition
			// is genuinely free of side effects.
			return m, nil
		}
		return nil, errdefs.Conflict(fmt.Sprintf("mission %q already finished as %q; cannot re-finish as %q", id, m.Status, status))
	}
	old := m.Status
	m.Status = status
	m.StatusReason = strings.TrimSpace(reason)
	m.UpdatedAt = time.Now().UTC()
	// Persist the durable fact FIRST, then announce it (the same order AddReport
	// takes): the terminal status is stored before anyone is told, so a lost or
	// failed publish never loses the outcome.
	if err := s.put(ctx, m, true); err != nil {
		return nil, err
	}
	s.publishStatusChanged(ctx, m, old)
	return m, nil
}

// SetPlan implements Service.SetPlan. It normalizes the incoming snapshot (trims
// content, assigns an id to any entry lacking one), validates its shape, enforces
// the completed-work immutability guard against the prior revision, then replaces
// the whole plan and bumps the revision.
//
// The normalization is why a fresh copy is built rather than mutating the
// caller's slice: SetPlan hands the caller back the stored entries (with their
// assigned ids and the new revision number) so the plan-tools slice can project
// exactly what was persisted, and doing that must not scribble ids into the
// caller's own PlanEntry values as a side effect.
func (s *service) SetPlan(ctx context.Context, id string, entries []PlanEntry, explanation string) (*Mission, error) {
	m, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	normalized := make([]PlanEntry, len(entries))
	for i, e := range entries {
		e.Content = strings.TrimSpace(e.Content)
		if strings.TrimSpace(e.ID) == "" {
			e.ID = uuid.NewString()
		}
		normalized[i] = e
	}

	if err := validatePlan(normalized); err != nil {
		return nil, err
	}
	if err := validateCompletedImmutable(m.Plan.Entries, normalized); err != nil {
		return nil, err
	}

	prev := m.Plan.Entries
	m.Plan = Plan{
		Entries:     normalized,
		Revision:    m.Plan.Revision + 1,
		Explanation: strings.TrimSpace(explanation),
	}
	now := time.Now().UTC()
	m.UpdatedAt = now

	// Build the revision summary ONCE and thread it into both the durable ring and
	// the (best-effort) event, so the persisted history and the published nudge
	// carry byte-identical numbers by construction — no chance of the two drifting.
	added, removed := planRevisionDelta(prev, m.Plan.Entries)
	pending, inProgress, completed := planStatusCounts(m.Plan.Entries)
	summary := PlanRevisionSummary{
		Revision:    m.Plan.Revision,
		Explanation: m.Plan.Explanation,
		Added:       added,
		Removed:     removed,
		Pending:     pending,
		InProgress:  inProgress,
		Completed:   completed,
		At:          now,
	}
	// The summary append is part of the DURABLE put, not the best-effort publish:
	// the history feed must survive an absent bus (see PlanRevisionSummary's
	// register). It is bounded to the last maxPlanRevisions so a long-lived,
	// heavily-replanned mission cannot grow its KV row without limit.
	m.PlanRevisions = appendPlanRevision(m.PlanRevisions, summary)

	if err := s.put(ctx, m, true); err != nil {
		return nil, err
	}
	s.publishPlanRevised(ctx, m, summary)
	return m, nil
}

// maxPlanRevisions bounds the durable revision ring: the last N summaries are
// kept (N=20, decision (b) of the component roadmap), oldest dropped first. It
// is deliberately small — the ring is a skim feed, not an audit log — and keeps
// the mission KV row bounded no matter how many times a plan is revised.
const maxPlanRevisions = 20

// appendPlanRevision appends s to the ring and trims it to the last
// maxPlanRevisions entries, oldest-first. When trimming, it copies into a fresh
// slice rather than resliced-in-place, so the returned ring never aliases a
// larger backing array that would keep dropped summaries reachable.
func appendPlanRevision(ring []PlanRevisionSummary, s PlanRevisionSummary) []PlanRevisionSummary {
	ring = append(ring, s)
	if len(ring) <= maxPlanRevisions {
		return ring
	}
	trimmed := make([]PlanRevisionSummary, maxPlanRevisions)
	copy(trimmed, ring[len(ring)-maxPlanRevisions:])
	return trimmed
}

// publishStatusChanged announces a terminal transition on StatusChangedSubject.
// BEST EFFORT, in the exact register of publishReportAdded: the terminal status
// is already the durable fact, so a publish failure must never turn a
// successfully-finished mission into a failed Finish. A no-op when no publisher
// was wired.
func (s *service) publishStatusChanged(ctx context.Context, m *Mission, old Status) {
	if s.pub == nil {
		return
	}
	ev := StatusChangedEvent{
		MissionID:       m.ID,
		ParentSessionID: m.ParentSessionID,
		AgentName:       m.AgentName,
		Intent:          m.Intent,
		OldStatus:       old,
		NewStatus:       m.Status,
		Reason:          m.StatusReason,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		slog.Warn("missionservice: marshal status-changed event failed; status stored, routing nudge skipped",
			"missionId", m.ID, "status", m.Status, "error", err)
		return
	}
	if err := s.pub.Publish(ctx, StatusChangedSubject, data); err != nil {
		slog.Warn("missionservice: publish status-changed event failed; status stored, routing nudge skipped",
			"missionId", m.ID, "status", m.Status, "error", err)
	}
}

// publishPlanRevised announces a stored plan snapshot on PlanRevisedSubject.
// BEST EFFORT, same register as publishReportAdded: the snapshot is already the
// durable fact (the plan AND its revision summary are persisted before this
// runs), so a publish failure must never fail SetPlan. A no-op when no publisher
// was wired. summary is the same value SetPlan already appended to the durable
// ring, so the event and the stored history carry identical numbers.
func (s *service) publishPlanRevised(ctx context.Context, m *Mission, summary PlanRevisionSummary) {
	if s.pub == nil {
		return
	}
	ev := PlanRevisedEvent{
		MissionID:       m.ID,
		ParentSessionID: m.ParentSessionID,
		AgentName:       m.AgentName,
		Intent:          m.Intent,
		Revision:        summary.Revision,
		Explanation:     summary.Explanation,
		EntryCount:      len(m.Plan.Entries),
		Added:           summary.Added,
		Removed:         summary.Removed,
		Pending:         summary.Pending,
		InProgress:      summary.InProgress,
		Completed:       summary.Completed,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		slog.Warn("missionservice: marshal plan-revised event failed; plan stored, routing nudge skipped",
			"missionId", m.ID, "revision", m.Plan.Revision, "error", err)
		return
	}
	if err := s.pub.Publish(ctx, PlanRevisedSubject, data); err != nil {
		slog.Warn("missionservice: publish plan-revised event failed; plan stored, routing nudge skipped",
			"missionId", m.ID, "revision", m.Plan.Revision, "error", err)
	}
}

// planRevisionDelta reports how many entry ids the new snapshot adds and drops
// relative to prev — the "+added/−removed" the inbox shows. It is keyed on id,
// so a status/priority/content edit to an entry that keeps its id is neither an
// add nor a drop; only genuinely new or genuinely gone entries count.
func planRevisionDelta(prev, next []PlanEntry) (added, removed int) {
	prevIDs := make(map[string]bool, len(prev))
	for _, e := range prev {
		if e.ID != "" {
			prevIDs[e.ID] = true
		}
	}
	nextIDs := make(map[string]bool, len(next))
	for _, e := range next {
		if e.ID != "" {
			nextIDs[e.ID] = true
		}
	}
	for id := range nextIDs {
		if !prevIDs[id] {
			added++
		}
	}
	for id := range prevIDs {
		if !nextIDs[id] {
			removed++
		}
	}
	return added, removed
}

// planStatusCounts tallies a snapshot by entry status, for the progress the
// board and inbox render. Entries with an unrecognized status cannot occur here
// (validatePlan rejects them before persistence), so the three known buckets
// account for every stored entry.
func planStatusCounts(entries []PlanEntry) (pending, inProgress, completed int) {
	for _, e := range entries {
		switch e.Status {
		case PlanEntryPending:
			pending++
		case PlanEntryInProgress:
			inProgress++
		case PlanEntryCompleted:
			completed++
		}
	}
	return pending, inProgress, completed
}

// Plan validation limits, ported from the retired planservice.planner_validate
// (recovered at 0c28a69^). They are DEFENSIVE, not aesthetic: they exist to keep
// a single hallucinated or stream-corrupted planner turn from writing a
// multi-megabyte KV row or pasting a build-tool stream into the plan, not to
// impose a house style on how a plan should read. Per the blueprint, host-side
// validation is HARD on shape and SOFT on discipline — hence a cap on count and
// size and a garbage detector, but no rule about how many steps may be
// in_progress at once.
const (
	maxPlanEntries     = 100
	maxPlanEntryBytes  = 12000
	planEscapeRatioNum = 3   // reject when backslashes exceed len/ratio (RSC/stream leak)
	planEscapeMinLen   = 400 // …but only past this length, so short escaped strings pass
)

// validatePlan checks a normalized (trimmed, id-assigned) snapshot for shape:
// non-empty, within the count cap, and every entry non-empty, within the size
// cap, not obvious garbage, and carrying a known status and priority. An empty
// snapshot is rejected outright — a plan with no entries is the degenerate case
// the old validator called "no steps", and full-snapshot-replace does not need
// an "erase the plan" path (deletion is omission of individual entries, not a
// wholesale empty write).
func validatePlan(entries []PlanEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("a plan must have at least one entry")
	}
	if len(entries) > maxPlanEntries {
		return fmt.Errorf("plan has too many entries (%d, max %d)", len(entries), maxPlanEntries)
	}
	for i := range entries {
		e := &entries[i]
		if e.Content == "" {
			return fmt.Errorf("plan entry %d has empty content", i+1)
		}
		if len(e.Content) > maxPlanEntryBytes {
			return fmt.Errorf("plan entry %d exceeds max length (%d bytes, max %d)", i+1, len(e.Content), maxPlanEntryBytes)
		}
		if planContentLooksCorrupted(e.Content) {
			return fmt.Errorf("plan entry %d looks corrupted (stream or log paste); revise the plan or shorten the step", i+1)
		}
		if err := validatePlanEntryStatus(e.Status); err != nil {
			return fmt.Errorf("plan entry %d: %w", i+1, err)
		}
		if err := validatePlanEntryPriority(e.Priority); err != nil {
			return fmt.Errorf("plan entry %d: %w", i+1, err)
		}
	}
	return nil
}

// planContentLooksCorrupted detects accidental inclusion of framework build
// streams or similar (a Next.js flight stream, an RSC dump) pasted into a step —
// ported verbatim in spirit from plannerStepLooksCorrupted: the __next_f marker,
// or an implausible backslash density past a minimum length.
func planContentLooksCorrupted(s string) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "__next_f") || strings.Contains(lower, "self.__next_f") {
		return true
	}
	if len(s) >= planEscapeMinLen {
		if strings.Count(s, "\\")*planEscapeRatioNum > len(s) {
			return true
		}
	}
	return false
}

// validateCompletedImmutable enforces blueprint pattern 5: a revision may not
// rewrite the content of work that was already completed. It keys on entry id —
// for every entry the PRIOR snapshot marked completed, if the NEW snapshot still
// carries that id its content must be identical.
//
// The scope of this check is a deliberate, documented decision. It guards the
// one thing the audit story cannot tolerate — silently changing what a finished
// step SAID — and nothing more. It does NOT forbid dropping a completed entry
// (deletion is omission, pattern 4) and does NOT police status transitions
// (reopening a completed step is discipline, left soft, pattern 3). A planner
// that wants to correct completed work appends a NEW entry (a fresh id, which
// this check ignores); it may not mutate the old one's text in place. That keeps
// the plan a trustworthy record of what was actually done without turning the
// runtime into a plan-discipline enforcer.
func validateCompletedImmutable(prev, next []PlanEntry) error {
	if len(prev) == 0 {
		return nil
	}
	completed := make(map[string]string, len(prev))
	for _, e := range prev {
		if e.Status == PlanEntryCompleted && e.ID != "" {
			completed[e.ID] = e.Content
		}
	}
	if len(completed) == 0 {
		return nil
	}
	for i := range next {
		e := &next[i]
		if e.ID == "" {
			continue
		}
		if prevContent, ok := completed[e.ID]; ok && e.Content != prevContent {
			return fmt.Errorf("plan entry %q rewrites the content of already-completed work; append a correction as a new entry instead", e.ID)
		}
	}
	return nil
}

func validatePlanEntryStatus(status PlanEntryStatus) error {
	switch status {
	case PlanEntryPending, PlanEntryInProgress, PlanEntryCompleted:
		return nil
	default:
		return fmt.Errorf("invalid plan entry status %q: must be one of pending|in_progress|completed", status)
	}
}

func validatePlanEntryPriority(priority PlanEntryPriority) error {
	switch priority {
	case PlanEntryPriorityHigh, PlanEntryPriorityMedium, PlanEntryPriorityLow:
		return nil
	default:
		return fmt.Errorf("invalid plan entry priority %q: must be one of high|medium|low", priority)
	}
}

func validate(m *Mission) error {
	if strings.TrimSpace(m.Intent) == "" {
		return fmt.Errorf("intent is required")
	}
	if strings.ContainsAny(m.Intent, "\r\n") {
		return fmt.Errorf("intent must be a single line")
	}
	if strings.TrimSpace(m.HITLPolicyName) == "" {
		return fmt.Errorf("hitlPolicyName is required: a mission must name its envelope")
	}
	return validateStatus(m.Status)
}

func validateStatus(status Status) error {
	switch status {
	case StatusOpen, StatusLanded, StatusDerailed, StatusStuck, StatusAbandoned:
		return nil
	default:
		return fmt.Errorf("invalid status %q: must be one of open|landed|derailed|stuck|abandoned", status)
	}
}

func validateReport(report *Report) error {
	if err := validateReportKind(report.Kind); err != nil {
		return err
	}
	if strings.TrimSpace(report.Summary) == "" {
		return fmt.Errorf("summary is required")
	}
	if strings.ContainsAny(report.Summary, "\r\n") {
		return fmt.Errorf("summary must be a single line")
	}
	return validateHandover(report.Handover)
}

// Hand-off validation limits, in the same DEFENSIVE register as the plan caps
// above (maxPlanEntries etc.): they exist to keep one hallucinated or
// stream-corrupted report from writing a multi-megabyte KV row or pasting a build
// stream into a hand-off field — not to impose a house style on what a good
// hand-off says. Hard on shape (size/count and the same corruption heuristic the
// plan uses), silent on substance.
const (
	maxHandoverTextBytes     = 8000 // per free-text field (outcome/handoverForNext/caveats)
	maxHandoverArtifacts     = 50   // artifacts listed on one hand-off
	maxHandoverArtifactBytes = 2000 // per artifact reference (a path or URL)
)

// validateHandover checks a report's OPTIONAL hand-off for shape. A nil hand-off
// is valid (a legacy report, or one with none) and validates to nothing. Present,
// each free-text field is capped and run through the plan's stream-leak detector,
// and the artifact list is capped in count and per-entry length. It is deliberately
// SILENT on whether a hand-off is well-written — that is the planner prompt's job,
// exactly as SetPlan is soft on plan discipline.
func validateHandover(h *Handover) error {
	if h == nil {
		return nil
	}
	for _, f := range []struct {
		name  string
		value string
	}{
		{"outcome", h.Outcome},
		{"handoverForNext", h.HandoverForNext},
		{"caveats", h.Caveats},
	} {
		if len(f.value) > maxHandoverTextBytes {
			return fmt.Errorf("handover %s exceeds max length (%d bytes, max %d)", f.name, len(f.value), maxHandoverTextBytes)
		}
		if planContentLooksCorrupted(f.value) {
			return fmt.Errorf("handover %s looks corrupted (stream or log paste); shorten it or move it to an artifact reference", f.name)
		}
	}
	if len(h.Artifacts) > maxHandoverArtifacts {
		return fmt.Errorf("handover has too many artifacts (%d, max %d)", len(h.Artifacts), maxHandoverArtifacts)
	}
	for i, a := range h.Artifacts {
		if len(a) > maxHandoverArtifactBytes {
			return fmt.Errorf("handover artifact %d exceeds max length (%d bytes, max %d)", i+1, len(a), maxHandoverArtifactBytes)
		}
	}
	return nil
}

func validateReportKind(kind ReportKind) error {
	switch kind {
	case ReportKindProgress, ReportKindFinding, ReportKindBlocker, ReportKindResult:
		return nil
	default:
		return fmt.Errorf("invalid report kind %q: must be one of progress|finding|blocker|result", kind)
	}
}
