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
	"fmt"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
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
}

// Service is the durable operator inbox: append a landed report (Add), read the
// pending backlog newest-first (List). It is read+append only in this slice —
// report notices need no answer, so there is no Respond/dismiss here; that is a
// later slice's concern if one is ever wanted, mirroring how the approval inbox
// grew its answer route separately.
type Service interface {
	// Add records item as a durable inbox entry: it assigns an id and CreatedAt
	// when absent and persists it. A report notice, once added, is the durable
	// fact the inbox renders.
	Add(ctx context.Context, item *Item) error

	// List returns inbox items newest-first, bounded by limit (defaulting when
	// limit<=0). The slice is always non-nil, so an empty inbox renders as [].
	List(ctx context.Context, limit int) ([]*Item, error)
}

type service struct {
	db libdb.DBManager
}

// New creates an operator inbox backed by the given database manager, storing
// items in the shared KV table (the same backing missionservice uses).
func New(db libdb.DBManager) Service {
	return &service{db: db}
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
	return s.store().SetKV(ctx, inboxKVPrefix+item.ID, raw)
}

func (s *service) List(ctx context.Context, limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = 100
	}
	kvs, err := s.store().ListKVPrefix(ctx, inboxKVPrefix, nil, limit)
	if err != nil {
		return nil, err
	}
	items := make([]*Item, 0, len(kvs))
	for _, kv := range kvs {
		var it Item
		if err := json.Unmarshal(kv.Value, &it); err != nil {
			return nil, fmt.Errorf("inbox item %q: %w", kv.Key, err)
		}
		items = append(items, &it)
	}
	return items, nil
}

func validateReason(r Reason) error {
	switch r {
	case ReasonOperatorFired, ReasonParentGone:
		return nil
	default:
		return fmt.Errorf("invalid inbox reason %q: must be one of operator_fired|parent_gone", r)
	}
}
