// Package missionservice stores mission records: the durable, agent-reportable
// half of the fleet manager. A unit runs unattended inside a permission envelope
// and reports back through tools it holds only while on the mission. One mission
// binds exactly one session and one instance.
package missionservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/errdefs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
)

// EventPublisher is the narrow slice of the bus AddReport uses to announce a new report; libbus.Messenger satisfies it, and a missionservice built without one simply doesn't publish.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// ReportAddedSubject is the bus subject AddReport publishes a ReportAddedEvent on, which a routing service subscribes to for delivery to the mission's supervisor (a live parent session, or the operator inbox).
const ReportAddedSubject = "missionservice.events.report_added"

// ReportAddedEvent is the self-contained event AddReport publishes after a report is durably stored, letting a subscriber route it without reading anything back; ParentSessionID is the supervision edge — non-empty names the upstream session that fired the mission, empty means an operator fired it directly.
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

// PlanRevisedEvent is the self-contained event SetPlan publishes after a plan snapshot is durably stored; Added/Removed are a presentation delta keyed on entry id, computed against the prior revision, and are never load-bearing elsewhere.
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

// PlanRevisionSummary is one durable entry in a mission's bounded revision ring (Mission.PlanRevisions) — the "+2/-1 - why" line for a past SetPlan — written inside the durable SetPlan put so a mission with no bus wired still accrues its full history.
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

// StatusChangedEvent is the self-contained event Finish publishes after a mission reaches a terminal state; OldStatus/NewStatus name the transition, and Reason mirrors the persisted Mission.StatusReason.
type StatusChangedEvent struct {
	MissionID       string `json:"missionId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	Intent          string `json:"intent,omitempty"`
	OldStatus       Status `json:"oldStatus"`
	NewStatus       Status `json:"newStatus"`
	Reason          string `json:"reason,omitempty"`
}

// Status is a mission's lifecycle state — one running state (open) and a closed set of terminal states — contracted so the set only ever grows, never renames (existing KV rows carry these exact strings).
type Status string

const (
	StatusOpen      Status = "open"
	StatusLanded    Status = "landed"
	StatusDerailed  Status = "derailed"
	StatusAbandoned Status = "abandoned"

	// StatusStuck is a first-class terminal signal distinct from StatusDerailed — a discrete boundary (a loop, a judgement call) that asks for attention rather than a post-mortem; detecting "stuck" is the caller's business, the runtime only owns the status.
	StatusStuck Status = "stuck"
)

// IsTerminalStatus reports whether status is one a mission never moves off
// again, so a caller watching one to rest asks the same question Finish's guard
// does rather than keeping its own list.
func IsTerminalStatus(status Status) bool {
	switch status {
	case StatusLanded, StatusDerailed, StatusStuck, StatusAbandoned:
		return true
	default:
		return false
	}
}

func isTerminalStatus(status Status) bool { return IsTerminalStatus(status) }

const missionKVPrefix = "fleet:mission:"

const missionReportKVPrefix = "fleet:mission_report:"

// Mission is the durable record: a one-line intent fired at a declared agent, bound to exactly one session and one instance, bounded by an envelope (a named HITL policy).
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
	// PlanRevisions is the last-N revision summaries, oldest-first, bounded by maxPlanRevisions; nil on a never-planned or legacy mission.
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

// Report is a single dispatch from a unit on a mission; Refs is by reference only (paths, URLs) and never carries artifact content.
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

// Handover is the structured hand-off a mission attaches to a report so the next mission builds on real context; every field is optional and an all-empty Handover is normalized to nil (see AddReport).
type Handover struct {
	// Outcome is a one-line verdict of what this mission achieved.
	Outcome string `json:"outcome,omitempty"`
	// Artifacts are the deliverables the next mission consumes, by reference only.
	Artifacts []string `json:"artifacts,omitempty"`
	// HandoverForNext is the free-text brief: what to pick up, what to watch for.
	HandoverForNext string `json:"handoverForNext,omitempty"`
	// Caveats are known limitations or risks the next mission must not assume away.
	Caveats string `json:"caveats,omitempty"`
}

// IsEmpty reports whether a hand-off carries no substance, used by AddReport to normalize an all-empty Handover to nil.
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

// PlanEntryStatus is a plan entry's lifecycle state, contracted to be byte-for-byte libacp.PlanEntryStatus since a plan record projects to ACP as a full snapshot.
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

// PlanEntry is one line of a mission's plan; ID is the entry's stable identity across revisions, what SetPlan diffs against and what the completed-work immutability guard keys on.
type PlanEntry struct {
	ID       string            `json:"id"`
	Content  string            `json:"content"`
	Status   PlanEntryStatus   `json:"status"`
	Priority PlanEntryPriority `json:"priority"`
}

// Plan is a mission's living plan: an ordered list of entries owned by one planner, held as a reviewable record, never a schedule the runtime reads back to decide what runs next.
type Plan struct {
	Entries []PlanEntry `json:"entries"`
	// Revision counts successful SetPlan calls; 0 is never planned.
	Revision int `json:"revision"`
	// Explanation is the latest revision's rationale only.
	Explanation string `json:"explanation,omitempty"`
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

	GetBySession(ctx context.Context, sessionID string) (*Mission, error)

	List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Mission, error)
	Update(ctx context.Context, m *Mission) error
	Delete(ctx context.Context, id string) error

	// Bind attaches sessionID and/or instanceID to mission id; re-binding the id already carried is an idempotent no-op, binding a different id over one already set is errdefs.ErrConflict, and an unknown mission id surfaces as libdb.ErrNotFound.
	Bind(ctx context.Context, id string, sessionID, instanceID string) (*Mission, error)

	// Heartbeat stamps LastHeartbeat to now and sets LastError to lastErr (empty clears it), then persists; a mission already at rest is returned untouched, and an unknown mission id surfaces as libdb.ErrNotFound.
	Heartbeat(ctx context.Context, id string, lastErr string) (*Mission, error)

	// Finish moves mission id into a terminal state, records reason as StatusReason, and publishes a StatusChangedEvent; guarded so only a non-terminal mission may be finished to a valid terminal status, a same-status repeat is a no-op, a different-status repeat is errdefs.ErrConflict, and an unknown mission id surfaces as libdb.ErrNotFound.
	Finish(ctx context.Context, id string, status Status, reason string) (*Mission, error)

	// SweepAbandoned reclaims every open mission whose heartbeat has been silent longer than its own bound (StaleHeartbeatAfter, widened to the longest parked ask), finishing it as StatusAbandoned with a blocker report, and returns how many it reclaimed; idempotent, since a reclaimed mission is terminal and a mission finishing normally under the race keeps its own outcome.
	SweepAbandoned(ctx context.Context) (int, error)

	// SetPlan replaces mission id's plan with a full snapshot, bumps the revision counter, and publishes a PlanRevisedEvent; entries are validated for shape only, a snapshot may not rewrite the content of an already-completed entry (matched by id), and an unknown mission id surfaces as libdb.ErrNotFound.
	SetPlan(ctx context.Context, id string, entries []PlanEntry, explanation string) (*Mission, error)

	// AddReport validates report, assigns an id and CreatedAt when absent, binds it to missionID, and persists it; missionID must name an existing mission or the call surfaces libdb.ErrNotFound.
	AddReport(ctx context.Context, missionID string, report *Report) error

	// ListReports returns missionID's reports newest-first; the slice is always non-nil.
	ListReports(ctx context.Context, missionID string, limit int) ([]*Report, error)
}

type service struct {
	db      libdb.DBManager
	pub     EventPublisher
	tracker libtracker.ActivityTracker
}

// Option configures a mission service at construction.
type Option func(*service)

// WithEventPublisher wires the bus AddReport publishes ReportAddedEvent on; unset, AddReport stores the report and publishes nothing.
func WithEventPublisher(pub EventPublisher) Option {
	return func(s *service) { s.pub = pub }
}

// WithTracker wires the ActivityTracker the best-effort publish paths report to; unset or nil, the service tracks nothing.
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
	m, _, err := s.getWithSnapshot(ctx, id)
	return m, err
}

func (s *service) getWithSnapshot(ctx context.Context, id string) (*Mission, json.RawMessage, error) {
	if id == "" {
		return nil, nil, fmt.Errorf("id is required")
	}
	raw, err := s.store().GetKVRaw(ctx, missionKVPrefix+id)
	if err != nil {
		return nil, nil, err
	}
	var m Mission
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("mission %q: %w", id, err)
	}
	return &m, raw, nil
}

const scanPageSize = 200

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
		// Strictly-decreasing-cursor guard: defends against an identical-timestamp storm looping forever.
		if cursor != nil && !next.Before(*cursor) {
			return nil, libdb.ErrNotFound
		}
		cursor = next
	}
}

func (s *service) GetBySession(ctx context.Context, sessionID string) (*Mission, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	var cursor *time.Time
	for {
		batch, next, err := s.listPage(ctx, cursor, scanPageSize)
		if err != nil {
			return nil, err
		}
		for _, m := range batch {
			if m.SessionID == sessionID {
				return m, nil
			}
		}
		if len(batch) < scanPageSize || next == nil {
			return nil, libdb.ErrNotFound
		}
		if cursor != nil && !next.Before(*cursor) {
			return nil, libdb.ErrNotFound
		}
		cursor = next
	}
}

func (s *service) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Mission, error) {
	if limit <= 0 {
		limit = 100
	}
	missions, _, err := s.listPage(ctx, createdAtCursor, limit)
	return missions, err
}

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

func (s *service) Bind(ctx context.Context, id string, sessionID, instanceID string) (*Mission, error) {
	if sessionID == "" && instanceID == "" {
		return nil, fmt.Errorf("bind requires a sessionId or instanceId")
	}
	for attempt := 0; attempt < casAttempts; attempt++ {
		m, snapshot, err := s.getWithSnapshot(ctx, id)
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
		err = s.putIfUnchanged(ctx, m, snapshot)
		if errors.Is(err, libdb.ErrNotFound) {
			continue // the row moved under us; re-read and re-judge the conflict check.
		}
		if err != nil {
			return nil, err
		}
		return m, nil
	}
	return nil, errdefs.Conflict(fmt.Sprintf("mission %q is being written concurrently; bind gave up after %d attempts", id, casAttempts))
}

func (s *service) Heartbeat(ctx context.Context, id string, lastErr string) (*Mission, error) {
	for attempt := 0; attempt < casAttempts; attempt++ {
		m, snapshot, err := s.getWithSnapshot(ctx, id)
		if err != nil {
			return nil, err
		}
		if isTerminalStatus(m.Status) {
			// Liveness for a finished mission is meaningless; writing it would put an at-rest row back in motion.
			return m, nil
		}
		now := time.Now().UTC()
		m.LastHeartbeat = &now
		m.LastError = lastErr
		m.UpdatedAt = now
		err = s.putIfUnchanged(ctx, m, snapshot)
		if errors.Is(err, libdb.ErrNotFound) {
			continue // the row moved under us; re-read and re-judge.
		}
		if err != nil {
			return nil, err
		}
		return m, nil
	}
	return nil, errdefs.Conflict(fmt.Sprintf("mission %q is being written concurrently; heartbeat gave up after %d attempts", id, casAttempts))
}

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

func (s *service) putIfUnchanged(ctx context.Context, m *Mission, snapshot json.RawMessage) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal mission: %w", err)
	}
	return s.store().UpdateKVIfUnchanged(ctx, missionKVPrefix+m.ID, snapshot, raw)
}

const casAttempts = 5

func (s *service) AddReport(ctx context.Context, missionID string, report *Report) error {
	if missionID == "" {
		return fmt.Errorf("missionId is required")
	}
	if report == nil {
		return fmt.Errorf("report is required")
	}
	// Fetch (not just check): proves the mission exists and hands us the supervision edge, with no second read.
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

func (s *service) Finish(ctx context.Context, id string, status Status, reason string) (*Mission, error) {
	if !isTerminalStatus(status) {
		return nil, fmt.Errorf("cannot finish mission %q as %q: a terminal status is required (landed|derailed|stuck|abandoned)", id, status)
	}
	for attempt := 0; attempt < casAttempts; attempt++ {
		m, snapshot, err := s.getWithSnapshot(ctx, id)
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
		err = s.putIfUnchanged(ctx, m, snapshot)
		if errors.Is(err, libdb.ErrNotFound) {
			continue // the row moved under us; re-read and re-judge the guard.
		}
		if err != nil {
			return nil, err
		}
		s.publishStatusChanged(ctx, m, old)
		return m, nil
	}
	return nil, errdefs.Conflict(fmt.Sprintf("mission %q is being written concurrently; finishing as %q gave up after %d attempts", id, status, casAttempts))
}

func (s *service) SetPlan(ctx context.Context, id string, entries []PlanEntry, explanation string) (*Mission, error) {
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

	for attempt := 0; attempt < casAttempts; attempt++ {
		m, snapshot, err := s.getWithSnapshot(ctx, id)
		if err != nil {
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

		// Built once and threaded into both the durable ring and the event so they carry identical numbers.
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
		// Part of the durable put, not the best-effort publish: the history feed must survive an absent bus.
		m.PlanRevisions = appendPlanRevision(m.PlanRevisions, summary)

		err = s.putIfUnchanged(ctx, m, snapshot)
		if errors.Is(err, libdb.ErrNotFound) {
			continue // the row moved under us; re-read and re-judge the guard.
		}
		if err != nil {
			return nil, err
		}
		s.publishPlanRevised(ctx, m, summary)
		return m, nil
	}
	return nil, errdefs.Conflict(fmt.Sprintf("mission %q is being written concurrently; setting the plan gave up after %d attempts", id, casAttempts))
}

const maxPlanRevisions = 20

func appendPlanRevision(ring []PlanRevisionSummary, s PlanRevisionSummary) []PlanRevisionSummary {
	ring = append(ring, s)
	if len(ring) <= maxPlanRevisions {
		return ring
	}
	trimmed := make([]PlanRevisionSummary, maxPlanRevisions)
	copy(trimmed, ring[len(ring)-maxPlanRevisions:])
	return trimmed
}

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

const (
	maxPlanEntries     = 100
	maxPlanEntryBytes  = 12000
	planEscapeRatioNum = 3
	planEscapeMinLen   = 400
)

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

const (
	maxHandoverTextBytes     = 8000
	maxHandoverArtifacts     = 50
	maxHandoverArtifactBytes = 2000
)

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
