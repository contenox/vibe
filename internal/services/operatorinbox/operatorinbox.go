// Package operatorinbox is the durable attention surface for mission
// reports that reached no live supervising session — a sibling store to
// hitlservice's approval inbox (informational, not answerable), not an
// overloaded row in it. An item is appended by the router, announced on
// AddedSubject, and later acked by the operator; Ack sets a flag, never deletes.
package operatorinbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/google/uuid"
)

// inboxKVPrefix namespaces operator-inbox items in the KV store; each item
// is stored at inboxKVPrefix+ID. Shares no prefix with the mission or
// mission-report keys, so the prefix scans never collide.
const inboxKVPrefix = "fleet:operator_inbox:"

// AddedSubject is the bus subject a successfully-stored inbox Item is
// announced on, carrying the JSON of the stored Item. It is a best-effort
// nudge on top of the durable write: the Item is already stored by the time
// this publishes, a publish failure is swallowed (see Add), and a
// subscriber that was down misses the nudge but never the item.
const AddedSubject = "operatorinbox.events.added"

// ErrNotFound is returned by Get and Ack for an unknown id. It wraps
// libdb.ErrNotFound so both errors.Is(err, ErrNotFound) and
// errors.Is(err, libdb.ErrNotFound) succeed.
var ErrNotFound = fmt.Errorf("operatorinbox: item not found: %w", libdb.ErrNotFound)

// EventPublisher is the narrow slice of the event bus Add uses to announce
// a stored item. libbus.Messenger satisfies it.
type EventPublisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// Option configures an operator inbox at construction.
type Option func(*service)

// WithEventPublisher wires the bus Add announces stored items on
// (AddedSubject). Unset — the default — Add stores the item and publishes
// nothing.
func WithEventPublisher(pub EventPublisher) Option {
	return func(s *service) { s.pub = pub }
}

// WithTracker wires the ActivityTracker the best-effort publish path
// reports to (see publishAdded). Unset or nil: the inbox tracks nothing.
func WithTracker(tracker libtracker.ActivityTracker) Option {
	return func(s *service) {
		if tracker != nil {
			s.tracker = tracker
		}
	}
}

// Reason records why a report landed in the operator inbox rather than
// reaching a live supervising session.
type Reason string

const (
	// ReasonOperatorFired: the mission carried no parent session, fired directly by an operator.
	ReasonOperatorFired Reason = "operator_fired"
	// ReasonParentGone: the mission named a parent session, but none was live when the report arrived.
	ReasonParentGone Reason = "parent_gone"
)

// Item is one mission report that reached no live supervisor, plus the
// mission attribution needed to render and act on it without a second read.
// It embeds the canonical missionservice.Report rather than re-describing one.
type Item struct {
	ID        string `json:"id"`
	MissionID string `json:"missionId"`
	AgentName string `json:"agentName,omitempty"`
	Intent    string `json:"intent,omitempty"`
	// ParentSessionID is the unreachable supervisor for ReasonParentGone,
	// empty for ReasonOperatorFired.
	ParentSessionID string                `json:"parentSessionId,omitempty"`
	Reason          Reason                `json:"reason"`
	Report          missionservice.Report `json:"report"`
	CreatedAt       time.Time             `json:"createdAt"`

	// Acked/AckedAt record that an operator has seen this notice. Ack sets
	// the flag rather than deleting the row, so the evidence survives. Both
	// are omitempty, so a pre-existing item decodes with Acked false.
	Acked   bool       `json:"acked,omitempty"`
	AckedAt *time.Time `json:"ackedAt,omitempty"`
}

// Service is the durable operator inbox: append a landed report (Add), read
// the backlog (List/ListUnacked), read one item (Get), and mark one seen
// (Ack). There is no Respond — an informational notice has no answer, only "read".
type Service interface {
	// Add records item as a durable inbox entry, assigning an id and
	// CreatedAt when absent, and announces it on AddedSubject when a
	// publisher is wired (best effort; see publishAdded).
	Add(ctx context.Context, item *Item) error

	// List returns inbox items newest-first, bounded by limit (defaulting
	// when limit<=0). Never nil; includes acked items.
	List(ctx context.Context, limit int) ([]*Item, error)

	// ListUnacked is List restricted to unacknowledged items — the worklist
	// `contenox inbox` shows.
	ListUnacked(ctx context.Context, limit int) ([]*Item, error)

	// Get returns one item by id, or an error satisfying ErrNotFound.
	Get(ctx context.Context, id string) (*Item, error)

	// Ack marks one item acknowledged. Idempotent: acking an already-acked
	// item is a no-op success. Returns an error satisfying ErrNotFound for
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

// publishAdded announces a stored item on AddedSubject, best effort: the
// item is already durable by the time this runs, so a publish failure never
// fails Add. A no-op when no publisher is wired. raw is the same JSON that
// was stored.
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

// unackedScanPage is how many stored rows one ListUnacked pass reads.
// Acked-ness lives inside the JSON document, so it cannot be a WHERE clause;
// the scan over-reads to still return `limit` unacked items, and paging
// keeps that bounded rather than loading the whole inbox.
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
		// The cursor is strict created_at <, so this always moves backwards and the loop terminates.
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

// Ack marks an item read: a read-modify-write of the stored document,
// deliberately not a delete, so the record that a report reached no live
// supervisor survives. Acking twice is a no-op success.
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
	// UpdateKV, not SetKV: a vanished id must surface as a miss, not a resurrection.
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
