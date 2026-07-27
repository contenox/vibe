// Package operatorinbox is the durable attention surface for mission reports
// that reached NO live supervising session — the other half of the supervision
// edge (docs/development/blueprints/acp/fleet-consolidation.md, "Mission mode",
// M3). A mission fired from a chat session reports back INTO that session; a
// mission an operator fired directly has no such upstream, so its reports land
// here, where an operator reads "what came back from the missions I fired"
// without walking each mission by hand.
//
// # Why this is a distinct store, and not the approval inbox
//
// The blueprint names "one attention surface" and says the report inbox is
// "C2's approval inbox, extended." Taken literally — reports stored as
// hitlservice.HITLApproval rows — that reading BREAKS the approval store's one
// invariant: an approval is answered, expired, or pending (an answerable ask
// with a deadline, an OnTimeout, and a boolean resolution). A report is none of
// those. It is an informational notice (progress, finding, blocker, result) that
// needs a human's EYES, not a human's DECISION. Forcing it into an approval row
// would mean inventing a permanently-"resolved"/"no-answer" pseudo-state and
// empty tool/policy/rule columns, corrupting exactly the invariant C2 exists to
// keep true. So "extended" is realized as a SIBLING durable surface in the same
// REST/KV idiom, not as an overloaded approval row — a deliberate divergence
// from the literal reading, made because the literal reading is a category
// error. A UI slice can still present the two together; they are both "things
// needing the operator's eyes." (A decision, not an oversight.)
//
// # Why a durable store at all, and not a read model over mission reports
//
// A report is already durably stored against its mission (missionservice), so a
// tempting cheaper design is a read model: "list reports from parent-less
// missions." It cannot express the second thing that lands here. When a mission
// HAS a parent session but that session is GONE by the time the report arrives,
// the report still needs an operator — yet the mission's ParentSessionID is
// non-empty, so no filter over mission fields can distinguish "delivered to a
// live supervisor" from "the supervisor vanished." That distinction is a routing
// OUTCOME, knowable only at routing time, so it has to be WRITTEN. This store is
// that written outcome. It is not a second source of truth for the report's
// existence (the mission store stays canonical); each Item is a self-contained
// snapshot recording that this report reached no live supervisor, and why.
//
// # Landed, seen, kept
//
// An item is APPENDED by the router, ANNOUNCED on AddedSubject so a live surface
// can react without polling, and eventually ACKED by the operator who read it.
// Neither of the last two weakens the first: the announcement is best-effort on
// top of a completed write, and Ack sets a flag rather than deleting the row, so
// "this report reached no live supervisor" stays permanently answerable. The
// worklist view (ListUnacked) is a filter over that record, never a different
// store.
//
// Storage mirrors missionservice exactly — runtimetypes KV records under one
// prefix, listed newest-first by the store's prefix scan, zero migration — so
// this is the same storage MECHANISM the mission subsystem already uses, not a
// new kind of store. Nothing here touches a spawn/resolve/registry lookup path;
// the "no second mechanism" invariant guards against a parallel REGISTRY or BUS,
// which this is not.
package operatorinbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
)

// inboxKVPrefix namespaces operator-inbox items in the KV store; each item is
// stored at inboxKVPrefix+ID and the set is listed by scanning this prefix. It
// shares no string prefix with the mission or mission-report prefixes
// ("fleet:mission:", "fleet:mission_report:"), so the three prefix scans never
// collide.
const inboxKVPrefix = "fleet:operator_inbox:"

// AddedSubject is the bus subject a successfully-stored inbox Item is announced
// on, carrying the JSON of the stored Item as its payload. It follows the
// codebase's "<package>.events[.<verb>]" convention byte-for-byte
// (missionservice.ReportAddedSubject, taskengine.TaskEventSubjectAll).
//
// It exists so a LIVE surface — a running `contenox inbox watch`, a TUI pane —
// learns that something needs the operator's eyes without polling the KV scan.
// The seam is the same one missionservice keeps: this package publishes THAT an
// item landed and stays ignorant of who renders it.
//
// The signal is strictly a nudge on top of the durable write, never a substitute
// for it: the Item is already stored when this is published, a publish failure
// is swallowed (see Add), and a subscriber that was down misses the nudge but
// never the item — List/ListUnacked still return it. Same best-effort invariant
// reportrouter states for routing, applied one layer down.
const AddedSubject = "operatorinbox.events.added"

// ErrNotFound is returned by Get and Ack for an id no inbox item claims. It
// WRAPS libdb.ErrNotFound so both checks a caller might already be writing
// succeed — errors.Is(err, operatorinbox.ErrNotFound) for a caller that speaks
// this package, and errors.Is(err, libdb.ErrNotFound) for the codebase-wide
// store-miss check every other service surfaces (missionservice.Get, and the
// CLI's shared not-found handling).
var ErrNotFound = fmt.Errorf("operatorinbox: item not found: %w", libdb.ErrNotFound)

// EventPublisher is the NARROW slice of the event bus Add uses to announce a
// stored item. libbus.Messenger satisfies it. Declared here, rather than
// importing libbus, for the reason missionservice.EventPublisher gives: the
// package depends on the one verb it calls, and an inbox built without a bus
// (every caller before this seam existed) simply does not publish.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// Option configures an operator inbox at construction, so the optional
// dependencies this service has can be wired without changing New's signature
// for the callers that pass only a db.
type Option func(*service)

// WithEventPublisher wires the bus Add announces stored items on (AddedSubject).
// When unset — the default — Add stores the item and publishes nothing, which is
// exactly how an inbox built before this seam existed behaves.
func WithEventPublisher(pub EventPublisher) Option {
	return func(s *service) { s.pub = pub }
}

// WithTracker wires the ActivityTracker the best-effort publish path reports to
// (see publishAdded). Unset — or set to nil — the inbox tracks nothing: an item
// is stored whether or not anyone is watching.
func WithTracker(tracker libtracker.ActivityTracker) Option {
	return func(s *service) {
		if tracker != nil {
			s.tracker = tracker
		}
	}
}

// Reason records WHY a report landed in the operator inbox rather than reaching
// a live supervising session — the two cases the router distinguishes.
type Reason string

const (
	// ReasonOperatorFired: the mission carried no parent session, so an operator
	// fired it directly and its reports were always inbox-bound.
	ReasonOperatorFired Reason = "operator_fired"
	// ReasonParentGone: the mission named a parent session, but no live instance
	// owned it when the report arrived (the supervisor ended). The report falls
	// back here rather than being lost — the never-silently-drop-an-attention
	// invariant, applied to reports.
	ReasonParentGone Reason = "parent_gone"
)

// Item is one mission report that reached no live supervisor, plus the mission
// attribution needed to render and act on it WITHOUT a second read — the same
// "self-contained, unprojected row" shape the approval inbox uses. It embeds the
// canonical missionservice.Report rather than re-describing one, so the inbox and
// a mission's own report list stay the same shape.
type Item struct {
	ID string `json:"id"`
	// MissionID, AgentName, Intent attribute the report to its mission so the
	// inbox reads as "what came back, and from what work" on its own.
	MissionID string `json:"missionId"`
	AgentName string `json:"agentName,omitempty"`
	Intent    string `json:"intent,omitempty"`
	// ParentSessionID is the (now-unreachable) supervisor for ReasonParentGone,
	// empty for ReasonOperatorFired. Kept so an operator can see that a supervisor
	// was intended but missed.
	ParentSessionID string                `json:"parentSessionId,omitempty"`
	Reason          Reason                `json:"reason"`
	Report          missionservice.Report `json:"report"`
	CreatedAt       time.Time             `json:"createdAt"`

	// Acked/AckedAt record that an operator has SEEN this notice — the one state
	// transition an informational item has. They are the "read" mark a backlog
	// needs to stay a backlog instead of growing forever, and they are why Ack is
	// not a DELETE: the item is the written record that a report reached no live
	// supervisor (see the package doc), so acking must not erase the evidence.
	//
	// They live IN the stored JSON document rather than in an acked column
	// because that is how this store keeps item state at all — every field above
	// is a field of one KV blob, and a KV blob gains a field with no migration:
	// rows written before this existed decode with Acked false, which is the
	// correct reading of "nobody has acknowledged it". Both are omitempty, so an
	// unacked item serializes byte-identically to how it did before.
	Acked   bool       `json:"acked,omitempty"`
	AckedAt *time.Time `json:"ackedAt,omitempty"`
}

// Service is the durable operator inbox: append a landed report (Add), read the
// backlog newest-first (List/ListUnacked), read one item (Get), and mark one
// seen (Ack). A report notice still needs no ANSWER — there is no Respond here,
// and Ack is not one: it says "I have read this", which is the only state an
// informational notice has, and it is what keeps `contenox inbox` a shrinking
// worklist rather than an ever-growing log.
type Service interface {
	// Add records item as a durable inbox entry: it assigns an id and CreatedAt
	// when absent and persists it. A report notice, once added, is the durable
	// fact the inbox renders. On success it also announces the stored item on
	// AddedSubject when a publisher is wired — best effort, see the method.
	Add(ctx context.Context, item *Item) error

	// List returns inbox items newest-first, bounded by limit (defaulting when
	// limit<=0). The slice is always non-nil, so an empty inbox renders as [].
	// It returns ACKED items too — it is the full record.
	List(ctx context.Context, limit int) ([]*Item, error)

	// ListUnacked is List restricted to items no operator has acknowledged: the
	// WORKLIST view, and what a bare `contenox inbox` shows. It is a sibling verb
	// rather than a parameter on List so the existing callers of List keep their
	// exact call and meaning (the full record), and so the two intents read
	// differently at the call site.
	ListUnacked(ctx context.Context, limit int) ([]*Item, error)

	// Get returns one item by id, or an error satisfying ErrNotFound when no item
	// claims that id.
	Get(ctx context.Context, id string) (*Item, error)

	// Ack marks one item acknowledged — the operator has read it. It is
	// IDEMPOTENT (acking an already-acked item is a no-op success, so a retried
	// CLI call is not an error) and returns an error satisfying ErrNotFound for
	// an unknown id.
	Ack(ctx context.Context, id string) error
}

type service struct {
	db      libdb.DBManager
	pub     EventPublisher
	tracker libtracker.ActivityTracker
}

// New creates an operator inbox backed by the given database manager, storing
// items in the shared KV table (the same backing missionservice uses).
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

func (s *service) Add(ctx context.Context, item *Item) error {
	if item == nil {
		return fmt.Errorf("item is required")
	}
	if item.MissionID == "" {
		return fmt.Errorf("missionId is required")
	}
	if err := validateReason(item.Reason); err != nil {
		return err
	}
	if item.ID == "" {
		item.ID = uuid.NewString()
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("marshal inbox item: %w", err)
	}
	if err := s.store().SetKV(ctx, inboxKVPrefix+item.ID, raw); err != nil {
		return err
	}
	s.publishAdded(ctx, item, raw)
	return nil
}

// publishAdded announces a stored item on AddedSubject. BEST EFFORT, in the
// exact register of missionservice.publishReportAdded: the item is ALREADY the
// durable fact by the time this runs, so a publish failure must never turn a
// successful Add into a failed one — a report that reached no live supervisor
// would then be lost twice over. A no-op when no publisher was wired.
//
// raw is the same JSON that was stored, so the payload a subscriber decodes and
// the row a reader lists are byte-identical.
func (s *service) publishAdded(ctx context.Context, item *Item, raw json.RawMessage) {
	if s.pub == nil {
		return
	}
	if err := s.pub.Publish(ctx, AddedSubject, raw); err != nil {
		reportErr, _, end := s.tracker.Start(ctx, "publish", "inbox_item_added_event", "itemId", item.ID, "missionId", item.MissionID)
		reportErr(fmt.Errorf("operatorinbox: publish item-added event failed; item stored, live nudge skipped: %w", err))
		end()
	}
}

func (s *service) List(ctx context.Context, limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	kvs, err := s.store().ListKVPrefix(ctx, inboxKVPrefix, nil, limit)
	if err != nil {
		return nil, err
	}
	items := make([]*Item, 0, len(kvs))
	for _, kv := range kvs {
		it, err := decodeItem(kv.Key, kv.Value)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

// defaultListLimit bounds a List/ListUnacked call that names no limit.
const defaultListLimit = 100

// unackedScanPage is how many stored rows one ListUnacked pass reads. Acked-ness
// lives inside the JSON document (see Item.Acked), so it cannot be a WHERE
// clause over a store that must speak both SQLite and Postgres — the filter runs
// in Go, and the scan therefore has to over-read to still return `limit` UNACKED
// items once a backlog has been partly worked through. Paging (rather than one
// big read) keeps that honest without loading an unbounded inbox into memory.
const unackedScanPage = 200

func (s *service) ListUnacked(ctx context.Context, limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	items := make([]*Item, 0, limit)
	var cursor *time.Time
	for len(items) < limit {
		kvs, err := s.store().ListKVPrefix(ctx, inboxKVPrefix, cursor, unackedScanPage)
		if err != nil {
			return nil, err
		}
		if len(kvs) == 0 {
			break
		}
		for _, kv := range kvs {
			it, err := decodeItem(kv.Key, kv.Value)
			if err != nil {
				return nil, err
			}
			if it.Acked {
				continue
			}
			items = append(items, it)
			if len(items) == limit {
				break
			}
		}
		if len(kvs) < unackedScanPage {
			break
		}
		// The store's cursor is a strict created_at <, so advancing to the oldest
		// row of this page moves strictly backwards and the loop always terminates.
		last := kvs[len(kvs)-1].CreatedAt
		cursor = &last
	}
	return items, nil
}

func (s *service) Get(ctx context.Context, id string) (*Item, error) {
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	var it Item
	if err := s.store().GetKV(ctx, inboxKVPrefix+id, &it); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, err
	}
	return &it, nil
}

// Ack marks an item read. It is a read-modify-write of the stored document —
// the store's own idiom for item state, since an Item IS one JSON blob — and
// deliberately NOT a delete: the inbox row is the written record that a report
// reached no live supervisor (the package doc's whole reason for existing), and
// that record must survive the operator noticing it. Acking twice is a no-op
// success, so a retried CLI call never reads as a failure.
func (s *service) Ack(ctx context.Context, id string) error {
	item, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if item.Acked {
		return nil
	}
	now := time.Now().UTC()
	item.Acked = true
	item.AckedAt = &now
	raw, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("marshal inbox item: %w", err)
	}
	// UpdateKV, not SetKV: an id that vanished between the read and the write
	// must surface as a miss rather than silently INSERTing a resurrected item.
	if err := s.store().UpdateKV(ctx, inboxKVPrefix+id, raw); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return err
	}
	return nil
}

func decodeItem(key string, raw json.RawMessage) (*Item, error) {
	var it Item
	if err := json.Unmarshal(raw, &it); err != nil {
		return nil, fmt.Errorf("inbox item %q: %w", key, err)
	}
	return &it, nil
}

func validateReason(r Reason) error {
	switch r {
	case ReasonOperatorFired, ReasonParentGone:
		return nil
	default:
		return fmt.Errorf("invalid inbox reason %q: must be one of operator_fired|parent_gone", r)
	}
}
