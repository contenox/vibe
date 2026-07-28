// Package missionservice stores mission records: the durable,
// agent-reportable half of the fleet manager. An
// operator fires a one-line intent at a declared agent; the resulting unit
// runs unattended inside a permission envelope (a named HITL policy bound to
// the mission) and reports back through tools it holds only while on the
// mission. One mission binds exactly one session and one instance.
package missionservice

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/beam/internal/errdefs"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
)

// EventPublisher is the narrow slice of the bus AddReport uses to announce a
// new report. libbus.Messenger satisfies it; declaring it here means a
// missionservice built without a bus simply doesn't publish.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// ReportAddedSubject is the bus subject AddReport publishes a
// ReportAddedEvent on. A routing service subscribes to deliver the report to
// the mission's supervisor (a live parent session, or the operator inbox).
const ReportAddedSubject = "missionservice.events.report_added"

// ReportAddedEvent is the self-contained event AddReport publishes after a
// report is durably stored; a subscriber routes it without reading anything
// back. ParentSessionID is the supervision edge: non-empty names the
// upstream session that fired the mission (route the report there), empty
// means an operator fired it directly (route to the operator inbox).
type ReportAddedEvent struct {
	MissionID       string `json:"missionId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	Report          Report `json:"report"`
}

// PlanRevisedSubject and StatusChangedSubject are the sibling bus subjects
// this package publishes plan and terminal-transition events on, so the
// inbox and board can subscribe without missionservice knowing who listens.
const (
	PlanRevisedSubject   = "missionservice.events.plan_revised"
	StatusChangedSubject = "missionservice.events.status_changed"
)

// PlanRevisedEvent is the self-contained event SetPlan publishes after a plan
// snapshot is durably stored. Added/Removed are a presentation delta keyed on
// entry id, computed against the prior revision — meaningful for the
// human-facing "what changed" line, never load-bearing elsewhere.
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

// PlanRevisionSummary is one durable entry in a mission's bounded revision
// ring (Mission.PlanRevisions): the "+2/-1 - why" line for a past SetPlan,
// kept so the inbox feed can show plan history, not just the current plan.
// It is written inside the durable SetPlan put (not merely published), so a
// mission that ran with no bus wired still accrues its full history.
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

// StatusChangedEvent is the self-contained event Finish publishes after a
// mission reaches a terminal state. OldStatus/NewStatus name the transition;
// Reason mirrors the persisted Mission.StatusReason.
type StatusChangedEvent struct {
	MissionID       string `json:"missionId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	OldStatus       Status `json:"oldStatus"`
	NewStatus       Status `json:"newStatus"`
	Reason          string `json:"reason,omitempty"`
}

// Status is a mission's lifecycle state: one running state (open) and a
// closed set of terminal states. The values are contracted — existing KV
// rows carry these exact strings, so the set only ever grows, never renames.
type Status string

const (
	StatusOpen      Status = "open"
	StatusLanded    Status = "landed"
	StatusDerailed  Status = "derailed"
	StatusAbandoned Status = "abandoned"

	// StatusStuck is a first-class terminal signal, distinct from
	// StatusDerailed: a discrete boundary (a loop, a judgement call) that
	// asks for attention rather than a post-mortem. Detecting "stuck" is the
	// caller's business; the runtime only owns the status.
	StatusStuck Status = "stuck"
)

// isTerminalStatus reports whether a mission in this state is finished — at
// rest in a closed terminal state, never to move again through Finish.
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

// missionReportKVPrefix namespaces mission reports, stored at
// missionReportKVPrefix+missionID+":"+reportID. It is deliberately a sibling
// of missionKVPrefix rather than a child of it: nesting reports under
// "fleet:mission:" would make List()'s prefix scan match report rows too.
const missionReportKVPrefix = "fleet:mission_report:"

// Mission is the durable record: a one-line intent fired at a declared
// agent, bound to exactly one session and one instance, bounded by an
// envelope (a named HITL policy). It may outlive its session/instance and
// stays listed while open.
//
// LastHeartbeat/LastError are liveness facts, not status: a mission stays
// StatusOpen for its whole run: see Heartbeat. ParentSessionID is the
// supervision edge — the upstream session that fired this mission, empty
// when an operator fired it directly; this layer only records the edge,
// reportrouter consumes it. Plan/PlanRevisions default to their zero value
// for a never-planned or legacy mission. StatusReason is the one line Finish
// attaches to a terminal transition.
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
	// PlanRevisions is the last-N revision summaries, oldest-first. Bounded by
	// maxPlanRevisions; nil on a never-planned or legacy mission.
	PlanRevisions []PlanRevisionSummary `json:"planRevisions,omitempty"`
	LastHeartbeat *time.Time            `json:"lastHeartbeat,omitempty"`
	LastError     string                `json:"lastError,omitempty"`
	CreatedAt     time.Time             `json:"createdAt"`
	UpdatedAt     time.Time             `json:"updatedAt"`
}

// ReportKind is the closed, small set of things a mission report may be
// (progress, finding, blocker, result) rather than free text — a short enum
// hints an unattended agent what is worth reporting at all.
type ReportKind string

const (
	ReportKindProgress ReportKind = "progress"
	ReportKindFinding  ReportKind = "finding"
	ReportKindBlocker  ReportKind = "blocker"
	ReportKindResult   ReportKind = "result"
)

// Report is a single dispatch from a unit on a mission. Refs is by
// reference only (paths, URLs) — a report never carries artifact content.
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

// Handover is the structured hand-off a mission attaches to a report so the
// next mission builds on real context. Every field is optional; an
// all-empty Handover is normalized to nil (see AddReport).
//
//   - Outcome: one-line verdict of what this mission achieved.
//   - Artifacts: deliverables the next mission consumes, by reference only —
//     distinct from Refs, which support this report.
//   - HandoverForNext: free-text brief — what to pick up, what to watch for.
//   - Caveats: known limitations or risks the next mission must not assume away.
type Handover struct {
	Outcome         string   `json:"outcome,omitempty"`
	Artifacts       []string `json:"artifacts,omitempty"`
	HandoverForNext string   `json:"handoverForNext,omitempty"`
	Caveats         string   `json:"caveats,omitempty"`
}

// IsEmpty reports whether a hand-off carries no substance. AddReport uses it
// to normalize an all-empty Handover to nil.
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

// PlanEntryStatus is a plan entry's lifecycle state. Its values are
// contracted to be byte-for-byte libacp.PlanEntryStatus (a conformance test
// there pins them), since a plan record projects to ACP as a full snapshot.
type PlanEntryStatus string

const (
	PlanEntryPending    PlanEntryStatus = "pending"
	PlanEntryInProgress PlanEntryStatus = "in_progress"
	PlanEntryCompleted  PlanEntryStatus = "completed"
)

// PlanEntryPriority mirrors libacp.PlanEntryPriority the same way.
type PlanEntryPriority string

const (
	PlanEntryPriorityHigh   PlanEntryPriority = "high"
	PlanEntryPriorityMedium PlanEntryPriority = "medium"
	PlanEntryPriorityLow    PlanEntryPriority = "low"
)

// PlanEntry is one line of a mission's plan. ID is the entry's stable
// identity across revisions — what a full-snapshot replace (SetPlan) diffs
// against, and what the completed-work immutability guard keys on.
type PlanEntry struct {
	ID       string            `json:"id"`
	Content  string            `json:"content"`
	Status   PlanEntryStatus   `json:"status"`
	Priority PlanEntryPriority `json:"priority"`
}

// Plan is a mission's living plan: an ordered list of entries owned by one
// planner, held as a reviewable record, never a schedule the runtime reads
// back to decide what runs next.
//
// Revision counts successful SetPlan calls: 0 is "never planned". Explanation
// is the latest revision's rationale only; the record keeps just the current
// snapshot.
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
	// libdb.ErrNotFound when no mission claims it (a normal answer for a
	// fleet unit brought up outside a dispatch).
	GetByInstance(ctx context.Context, instanceID string) (*Mission, error)

	List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Mission, error)
	Update(ctx context.Context, m *Mission) error
	Delete(ctx context.Context, id string) error

	// Bind attaches sessionID and/or instanceID to mission id. Re-binding the
	// id a mission already carries is an idempotent no-op; binding a
	// different id over one already set is errdefs.ErrConflict. An unknown
	// mission id surfaces as libdb.ErrNotFound.
	Bind(ctx context.Context, id string, sessionID, instanceID string) (*Mission, error)

	// Heartbeat stamps LastHeartbeat to now and sets LastError to lastErr
	// (empty clears it), then persists. An unknown mission id surfaces as
	// libdb.ErrNotFound.
	Heartbeat(ctx context.Context, id string, lastErr string) (*Mission, error)

	// Finish moves mission id into a terminal state, records reason as
	// StatusReason, and publishes a StatusChangedEvent. Guarded: only a
	// non-terminal mission may be finished, only a terminal status is a
	// valid target. A second Finish naming the same terminal status is an
	// idempotent no-op; a different terminal status over an already-finished
	// mission is errdefs.ErrConflict. An unknown mission id surfaces as
	// libdb.ErrNotFound.
	Finish(ctx context.Context, id string, status Status, reason string) (*Mission, error)

	// SetPlan replaces mission id's plan with a full snapshot, bumps the
	// revision counter, and publishes a PlanRevisedEvent. Entries are
	// validated for shape, not planning discipline. The one audit-safety
	// guard enforced: a snapshot may not rewrite the content of an entry
	// already completed in the prior revision (matched by id) — corrections
	// are appended as new entries. An unknown mission id surfaces as
	// libdb.ErrNotFound.
	SetPlan(ctx context.Context, id string, entries []PlanEntry, explanation string) (*Mission, error)

	// AddReport validates report, assigns an id and CreatedAt when absent,
	// binds it to missionID, and persists it. missionID must name an
	// existing mission — an unknown one surfaces as libdb.ErrNotFound.
	AddReport(ctx context.Context, missionID string, report *Report) error

	// ListReports returns missionID's reports newest-first. The slice is
	// always non-nil.
	ListReports(ctx context.Context, missionID string, limit int) ([]*Report, error)
}

type service struct {
	db      libdb.DBManager
	pub     EventPublisher
	tracker libtracker.ActivityTracker
}

// Option configures a mission service at construction.
type Option func(*service)

// WithEventPublisher wires the bus AddReport publishes ReportAddedEvent on.
// Unset, AddReport stores the report and publishes nothing.
func WithEventPublisher(pub EventPublisher) Option {
	return func(s *service) { s.pub = pub }
}

// WithTracker wires the ActivityTracker the best-effort publish paths report
// to. Unset or nil, the service tracks nothing.
func WithTracker(tracker libtracker.ActivityTracker) Option {
	return func(s *service) {
		if tracker != nil {
			s.tracker = tracker
		}
	}
}

// New creates a mission service backed by the given database manager.
func New(db libdb.DBManager, opts ...Option) Service {
	s := &service{db: db, tracker: libtracker.NoopTracker{}}
	for _, opt := range opts {
		opt(s)
	}
	if s.tracker == nil {
		s.tracker = libtracker.NoopTracker{}
	}
	return s
}

func (s *service) store() runtimetypes.Store {
	return runtimetypes.New(s.db.WithoutTransaction())
}

// Create validates m, assigns an id when absent, forces the status to open,
// stamps timestamps, and persists it. A mission with no HITLPolicyName is
// rejected: the envelope is what bounds an unattended unit.
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

// scanPageSize bounds one page of the mission prefix scan GetByInstance
// walks. The scan is off the hot path (once per unattended permission
// request), so this bounds one query's result set rather than makes it fast.
const scanPageSize = 200

// GetByInstance scans mission records newest-first and returns the first
// whose InstanceID matches. A scan, not a secondary index, because an index
// would be a second source of truth for a fact the mission record already
// owns; the cost is bounded by n (missions are dispatches, one per unit of
// work). If two missions ever claim the same instance (only possible via a
// hand-written Bind), the newest match wins, deterministically.
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
		// Strictly-decreasing-cursor guard: defends against an
		// identical-timestamp storm looping forever, at the cost of
		// truncating such a tie.
		if cursor != nil && !next.Before(*cursor) {
			return nil, libdb.ErrNotFound
		}
		cursor = next
	}
}

// List returns missions newest-first via the store's prefix scan. The slice
// is always non-nil.
func (s *service) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Mission, error) {
	if limit <= 0 {
		limit = 100
	}
	missions, _, err := s.listPage(ctx, createdAtCursor, limit)
	return missions, err
}

// listPage is List's implementation plus the cursor for the next page: the
// store-side created_at of the oldest row returned, which is not quite
// Mission.CreatedAt (the mission stamps its own just before the write) —
// paging on the record's own timestamp would silently skip rows written in
// that gap.
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

// Update validates m and persists changes to an existing mission. An
// unknown id surfaces as libdb.ErrNotFound. The caller owns m's CreatedAt.
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

// Bind attaches sessionID and/or instanceID to mission id. Binding is not
// additive: setting an id the mission doesn't yet carry succeeds, re-setting
// the same id is an idempotent no-op, and setting a different id over one
// already bound is a conflict — a caller wanting a different unit dispatches
// a new mission instead. An unknown mission id surfaces as libdb.ErrNotFound.
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

// Heartbeat stamps LastHeartbeat to now, sets LastError to lastErr (empty
// clears it), and persists — an explicit, agent-reported liveness fact since
// nobody is watching an unattended mission's transcript. An unknown mission
// id surfaces as libdb.ErrNotFound.
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

// put marshals m and writes it to the KV store. When mustExist is true it
// uses UpdateKV, whose zero-rows-affected result surfaces as
// libdb.ErrNotFound rather than a silent insert.
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

// AddReport validates report, assigns an id and CreatedAt when absent, binds
// it to missionID (overriding whatever MissionID the caller supplied), and
// persists it. missionID is checked against the mission store first, so
// posting against an unknown mission is libdb.ErrNotFound, not a silent insert.
func (s *service) AddReport(ctx context.Context, missionID string, report *Report) error {
	if missionID == "" {
		return fmt.Errorf("missionId is required")
	}
	if report == nil {
		return fmt.Errorf("report is required")
	}
	// Fetch (not just check) the mission: proves it exists and hands us the
	// supervision edge the event carries, with no second read.
	m, err := s.Get(ctx, missionID)
	if err != nil {
		return err
	}
	report.MissionID = missionID
	// Collapse an all-empty hand-off to nil before validation/storage.
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
	// Persist first, then nudge: a lost or late publish never loses the report.
	if err := s.store().SetKV(ctx, missionReportKVPrefix+missionID+":"+report.ID, raw); err != nil {
		return err
	}
	s.publishReportAdded(ctx, m, report)
	return nil
}

// publishReportAdded announces a stored report on ReportAddedSubject.
// Best-effort and never surfaces to AddReport's caller: the report is
// already durable, so a publish failure must not fail AddReport. A no-op
// when no publisher was wired.
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
		reportErr, _, end := s.tracker.Start(ctx, "publish", "report_added_event", "missionId", m.ID, "reportId", report.ID)
		reportErr(fmt.Errorf("missionservice: marshal report-added event failed; report stored, routing nudge skipped: %w", err))
		end()
		return
	}
	if err := s.pub.Publish(ctx, ReportAddedSubject, data); err != nil {
		reportErr, _, end := s.tracker.Start(ctx, "publish", "report_added_event", "missionId", m.ID, "reportId", report.ID)
		reportErr(fmt.Errorf("missionservice: publish report-added event failed; report stored, routing nudge skipped: %w", err))
		end()
	}
}

// ListReports returns missionID's reports newest-first. The slice is always
// non-nil.
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

// Finish implements Service.Finish. The guard lives here rather than in
// Update because Update is the unguarded manual-override path (an operator
// correcting a mislabeled mission), while Finish is the agent-reportable,
// hard-fact path a finished mission must not be silently re-terminalized
// through. A retried Finish naming the same terminal status is a no-op; a
// different terminal status over an already-finished mission is a conflict.
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
			// Idempotent no-op: return the record untouched, no restamp, no event.
			return m, nil
		}
		return nil, errdefs.Conflict(fmt.Sprintf("mission %q already finished as %q; cannot re-finish as %q", id, m.Status, status))
	}
	old := m.Status
	m.Status = status
	m.StatusReason = strings.TrimSpace(reason)
	m.UpdatedAt = time.Now().UTC()
	// Persist first, then announce: a lost or failed publish never loses the outcome.
	if err := s.put(ctx, m, true); err != nil {
		return nil, err
	}
	s.publishStatusChanged(ctx, m, old)
	return m, nil
}

// SetPlan implements Service.SetPlan: normalizes the incoming snapshot
// (trims content, assigns ids to entries lacking one), validates shape,
// enforces the completed-work immutability guard against the prior
// revision, then replaces the plan and bumps the revision. A fresh copy is
// built rather than mutating the caller's slice, since the stored entries
// (with assigned ids and new revision) are handed back to the caller.
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

	// Built once and threaded into both the durable ring and the event, so
	// they carry byte-identical numbers by construction.
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
	// Part of the durable put, not the best-effort publish: the history feed
	// must survive an absent bus.
	m.PlanRevisions = appendPlanRevision(m.PlanRevisions, summary)

	if err := s.put(ctx, m, true); err != nil {
		return nil, err
	}
	s.publishPlanRevised(ctx, m, summary)
	return m, nil
}

// maxPlanRevisions bounds the durable revision ring: the last N summaries
// are kept, oldest dropped first, so the mission KV row stays bounded no
// matter how often a plan is revised.
const maxPlanRevisions = 20

// appendPlanRevision appends s to the ring and trims to the last
// maxPlanRevisions entries, copying into a fresh slice so the result never
// aliases a larger backing array that would keep dropped summaries reachable.
func appendPlanRevision(ring []PlanRevisionSummary, s PlanRevisionSummary) []PlanRevisionSummary {
	ring = append(ring, s)
	if len(ring) <= maxPlanRevisions {
		return ring
	}
	trimmed := make([]PlanRevisionSummary, maxPlanRevisions)
	copy(trimmed, ring[len(ring)-maxPlanRevisions:])
	return trimmed
}

// publishStatusChanged announces a terminal transition on
// StatusChangedSubject. Best-effort, same register as publishReportAdded.
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
		reportErr, _, end := s.tracker.Start(ctx, "publish", "status_changed_event", "missionId", m.ID, "status", m.Status)
		reportErr(fmt.Errorf("missionservice: marshal status-changed event failed; status stored, routing nudge skipped: %w", err))
		end()
		return
	}
	if err := s.pub.Publish(ctx, StatusChangedSubject, data); err != nil {
		reportErr, _, end := s.tracker.Start(ctx, "publish", "status_changed_event", "missionId", m.ID, "status", m.Status)
		reportErr(fmt.Errorf("missionservice: publish status-changed event failed; status stored, routing nudge skipped: %w", err))
		end()
	}
}

// publishPlanRevised announces a stored plan snapshot on PlanRevisedSubject.
// Best-effort, same register as publishReportAdded; summary is the same
// value already appended to the durable ring, so event and history match.
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
		reportErr, _, end := s.tracker.Start(ctx, "publish", "plan_revised_event", "missionId", m.ID, "revision", m.Plan.Revision)
		reportErr(fmt.Errorf("missionservice: marshal plan-revised event failed; plan stored, routing nudge skipped: %w", err))
		end()
		return
	}
	if err := s.pub.Publish(ctx, PlanRevisedSubject, data); err != nil {
		reportErr, _, end := s.tracker.Start(ctx, "publish", "plan_revised_event", "missionId", m.ID, "revision", m.Plan.Revision)
		reportErr(fmt.Errorf("missionservice: publish plan-revised event failed; plan stored, routing nudge skipped: %w", err))
		end()
	}
}

// planRevisionDelta reports how many entry ids the new snapshot adds and
// drops relative to prev, keyed on id — a content/status/priority edit that
// keeps its id is neither an add nor a drop.
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

// planStatusCounts tallies a snapshot by entry status. validatePlan rejects
// unrecognized statuses before persistence, so the three buckets account for
// every stored entry.
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

// Plan validation limits are defensive, not aesthetic: they keep one
// hallucinated or stream-corrupted planner turn from writing an oversized KV
// row, not impose a house style. Hard on shape, soft on discipline.
const (
	maxPlanEntries     = 100
	maxPlanEntryBytes  = 12000
	planEscapeRatioNum = 3   // reject when backslashes exceed len/ratio (RSC/stream leak)
	planEscapeMinLen   = 400 // …but only past this length, so short escaped strings pass
)

// validatePlan checks a normalized snapshot for shape: non-empty, within the
// count cap, and every entry non-empty, within the size cap, not obvious
// garbage, and carrying a known status and priority.
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

// planContentLooksCorrupted detects a framework build stream (a Next.js
// flight stream, an RSC dump) pasted into a step: the __next_f marker, or an
// implausible backslash density past a minimum length.
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

// validateCompletedImmutable enforces that a revision may not rewrite the
// content of work already completed in the prior snapshot (matched by id).
// It does not forbid dropping a completed entry or police status
// transitions — only silently changing what a finished step said. A
// correction is a new entry (fresh id), never a mutation of the old one.
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

// Hand-off validation limits, in the same defensive register as the plan
// caps above: hard on shape, silent on substance.
const (
	maxHandoverTextBytes     = 8000 // per free-text field (outcome/handoverForNext/caveats)
	maxHandoverArtifacts     = 50   // artifacts listed on one hand-off
	maxHandoverArtifactBytes = 2000 // per artifact reference (a path or URL)
)

// validateHandover checks a report's optional hand-off for shape. A nil
// hand-off is valid. Present, each free-text field is capped and run through
// the plan's stream-leak detector; the artifact list is capped in count and
// per-entry length.
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
