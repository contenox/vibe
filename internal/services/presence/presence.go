// Package presence lets an editor-spawned contenox process (`contenox acp`,
// `vscode-agent`, `serve` itself) self-register into the shared store so the
// fleet board can show it. A presence entry is observation only: the kernel
// owns no lifecycle over these processes, so it carries no Stop/Cancel
// verbs. Registration is crash-safe (TTL + heartbeat) and always best-effort.
package presence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/contenox/beam/internal/libkvstore"
)

// Kind is which contenox surface a process is — the primary way the board
// distinguishes an editor session from serve (and, with ClientName, from
// another editor).
type Kind string

const (
	// KindACP is a `contenox acp` stdio process (Zed, GoLand, AionUi, etc.); ClientName carries which one.
	KindACP Kind = "acp"
	// KindVSCodeAgent is a `contenox vscode-agent` stdio process.
	KindVSCodeAgent Kind = "vscode-agent"
	// KindServe is the `contenox serve` process itself.
	KindServe Kind = "serve"
)

// DefaultTTL is the staleness threshold: a record whose LastSeen is older
// than this is reported Stale.
const DefaultTTL = 30 * time.Second

// DefaultHeartbeatInterval is how often a Reporter renews its record (TTL/3).
const DefaultHeartbeatInterval = DefaultTTL / 3

// DefaultInitialDelay defers a Reporter's first registration write past the
// process's boot-critical window, so an eager presence write cannot starve
// schema/preset init into "database is locked" on a fresh boot.
const DefaultInitialDelay = 1500 * time.Millisecond

// ReapMultiple sets how long past its TTL a record survives (marked Stale)
// before its KV row ages out of the listing entirely: (ReapMultiple-1)×TTL.
const ReapMultiple = 3

// Record is one instance's presence, written to the shared store and
// renewed on each heartbeat: identity, kind, liveness timestamps, and a few
// observed facts. It carries no control surface — see the package doc.
type Record struct {
	// InstanceID is a stable per-process id (a uuid minted at start).
	InstanceID string `json:"instanceId"`
	Kind       Kind   `json:"kind"`
	// PID is informational only; liveness is enforced by TTL, never a process check.
	PID int `json:"pid"`
	// Host is the machine hostname, informational.
	Host string `json:"host,omitempty"`
	// StartedAt is when the process registered; stable across heartbeats.
	StartedAt time.Time `json:"startedAt"`
	// LastSeen is the last successful heartbeat, bumped on every renew.
	LastSeen time.Time `json:"lastSeen"`
	// Cwd is the process working directory (an editor's project dir), optional.
	Cwd string `json:"cwd,omitempty"`
	// Address is the reachable listen address of a serve process, set only
	// for KindServe; empty for the stdio editor kinds. Never carries the
	// bearer token, which a sibling instead reads from CONTENOX_SERVER_TOKEN.
	Address string `json:"address,omitempty"`
	// SessionCount is how many ACP sessions are open, best-effort.
	SessionCount int `json:"sessionCount"`
	// ClientName is the editor that spawned the process (from the ACP initialize handshake), when known.
	ClientName string `json:"clientName,omitempty"`
}

// Entry is the read shape the board consumes: a Record plus two derived
// facts, External and Stale.
type Entry struct {
	Record
	// External is always true: the kernel owns no lifecycle over a presence entry.
	External bool `json:"external"`
	// Stale reports that LastSeen is older than the store's staleness TTL.
	Stale bool `json:"stale"`
}

const keyPrefix = "presence:"

func recordKey(kind Kind, instanceID string) string {
	return keyPrefix + string(kind) + ":" + instanceID
}

// Store reads and writes presence records over a shared KV store: the one
// place the key layout, row TTL, and staleness derivation live.
type Store struct {
	kv       libkvstore.KVManager
	now      func() time.Time
	staleTTL time.Duration
	hardTTL  time.Duration
}

// StoreOption customizes a Store (chiefly for tests).
type StoreOption func(*Store)

// WithClock overrides the Store's clock, so a test can drive staleness/aging
// deterministically without sleeping.
func WithClock(now func() time.Time) StoreOption {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// WithStaleTTL overrides the staleness threshold (default DefaultTTL).
func WithStaleTTL(d time.Duration) StoreOption {
	return func(s *Store) {
		if d > 0 {
			s.staleTTL = d
		}
	}
}

// WithHardTTL overrides the KV row TTL after which a record ages out of the
// listing (default DefaultTTL×ReapMultiple).
func WithHardTTL(d time.Duration) StoreOption {
	return func(s *Store) {
		if d > 0 {
			s.hardTTL = d
		}
	}
}

// NewStore returns a Store over kv. kv is the shared-SQLite KV manager both the
// editor writers and serve's board reader hold against $HOME/.contenox/local.db.
func NewStore(kv libkvstore.KVManager, opts ...StoreOption) *Store {
	s := &Store{
		kv:       kv,
		now:      func() time.Time { return time.Now() },
		staleTTL: DefaultTTL,
		hardTTL:  DefaultTTL * ReapMultiple,
	}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	return s
}

// Register writes (or renews) rec with the store's row TTL: renewing is
// just writing again with a fresh LastSeen. Stamps LastSeen when the caller
// left it zero. A Reporter treats a returned error as best-effort and swallows it.
func (s *Store) Register(ctx context.Context, rec Record) error {
	if rec.InstanceID == "" {
		return errors.New("presence: instanceID is required")
	}
	if rec.Kind == "" {
		return errors.New("presence: kind is required")
	}
	if rec.LastSeen.IsZero() {
		rec.LastSeen = s.now().UTC()
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = rec.LastSeen
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("presence: marshal record: %w", err)
	}
	exec, err := s.kv.Executor(ctx)
	if err != nil {
		return fmt.Errorf("presence: kv executor: %w", err)
	}
	if err := exec.SetWithTTL(ctx, recordKey(rec.Kind, rec.InstanceID), b, s.hardTTL); err != nil {
		return fmt.Errorf("presence: write record: %w", err)
	}
	return nil
}

// List returns every live presence record, annotated External and Stale
// (LastSeen against the staleness TTL). A row whose KV TTL has lapsed is
// already absent; a corrupt or vanished-mid-read row is skipped rather than
// failing the whole listing. Sorted for a deterministic order.
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	exec, err := s.kv.Executor(ctx)
	if err != nil {
		return nil, fmt.Errorf("presence: kv executor: %w", err)
	}
	keys, err := exec.Keys(ctx, keyPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("presence: list keys: %w", err)
	}
	now := s.now()
	entries := make([]Entry, 0, len(keys))
	for _, k := range keys {
		raw, err := exec.Get(ctx, k)
		if errors.Is(err, libkvstore.ErrNotFound) {
			// Aged out between the Keys scan and this Get — not an error, just gone.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("presence: read %q: %w", k, err)
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			// A corrupt row must not blind the board to every healthy one.
			continue
		}
		entries = append(entries, Entry{
			Record:   rec,
			External: true,
			Stale:    now.Sub(rec.LastSeen) > s.staleTTL,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		if !entries[i].StartedAt.Equal(entries[j].StartedAt) {
			return entries[i].StartedAt.Before(entries[j].StartedAt)
		}
		return entries[i].InstanceID < entries[j].InstanceID
	})
	return entries, nil
}

// Deregister removes a record — the best-effort clean-shutdown path.
// Deleting an already-gone key is a no-op.
func (s *Store) Deregister(ctx context.Context, kind Kind, instanceID string) error {
	exec, err := s.kv.Executor(ctx)
	if err != nil {
		return fmt.Errorf("presence: kv executor: %w", err)
	}
	if err := exec.Delete(ctx, recordKey(kind, instanceID)); err != nil {
		return fmt.Errorf("presence: delete record: %w", err)
	}
	return nil
}

var _ ReporterStore = (*Store)(nil)
