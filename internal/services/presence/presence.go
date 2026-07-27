// Package presence makes the WHOLE fleet visible, not only the units the kernel
// dispatched. An editor-spawned contenox process — Zed's `contenox acp`, VS
// Code's `vscode-agent`, or `contenox serve` itself — self-registers a small,
// durable-ish record into the shared store so the fleet board can show it. These
// instances run in their own process, over their own stdio connection, sharing
// only $HOME/.contenox with everyone else; the kernel never spawned them and owns
// no lifecycle over them.
//
// # Presence is observation, never control
//
// A presence entry is something the board OBSERVES, not something it manages. The
// kernel cannot Stop or Cancel an editor's own process, so a presence entry
// carries NO such verbs and is marked External on the wire — the board renders it
// as "seen, not steered". This is the load-bearing distinction from a
// kernel-dispatched FleetEntry (runtime/agentinstance), which the kernel does own
// and can act on. Mixing the two would tempt the board to offer a Stop button the
// runtime cannot honor; keeping presence a distinct, verb-less section keeps the
// board honest about what it can actually do.
//
// # Crash-safe by construction, no deregistration required
//
// A record is written with a TTL and RENEWED on a heartbeat (a modest interval,
// plus on session events). Liveness is the last renewal, nothing else: a crashed
// editor simply stops renewing, its record is marked stale at TTL, and the shared
// store ages the row out entirely after N×TTL — no explicit deregistration is
// needed for correctness. A clean shutdown ALSO best-effort deregisters so a row
// vanishes promptly rather than lingering stale, but that is an optimization, not
// a requirement. This is the same liveness-not-safety shape liblease/owner use
// for the modeld daemon, adapted from single-holder to the many-holders a fleet
// of editors needs.
//
// # Best-effort, always
//
// Registration and every heartbeat are best-effort: a presence write that fails
// is a shrug reported to the tracker, never something that blocks or fails the
// process it observes. An editor that cannot write its presence still serves its
// user; it is merely invisible on the board until its next successful heartbeat.
//
// # Storage: keyed rows in the shared SQLite KV store
//
// Presence is inherently MANY records that a reader enumerates, so it rides the
// shared-SQLite KV store (libkvstore over $HOME/.contenox/local.db): one keyed
// row per instance, listed by glob, each with a native per-row TTL. That is the
// multi-holder shape a fleet needs — liblease is single-holder-per-path and would
// have to reinvent a directory scheme on top of its atomic-write helper to hold
// many rows. Both writers (`contenox acp`, `vscode-agent`) and the reader
// (`contenox serve`) already hold a handle to this one shared db, and the
// cross-process shared-SQLite path is already proven by the mission tools (a
// `contenox acp` unit writes mission reports that serve reads over the same db).
// The KV row's own TTL does the aging-out for free — a crashed editor's row
// disappears from the listing once it stops being renewed — so there is no reaper
// to run.
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

// Kind is what a presence record IS at runtime — which contenox surface the
// process is. It is the primary way the board distinguishes an editor session
// from serve, and (together with ClientName) from another editor.
type Kind string

const (
	// KindACP is a `contenox acp` stdio process — the ACP server Zed, GoLand,
	// AionUi and other editors spawn. ClientName carries which one (from the ACP
	// initialize handshake), because they all run the same subcommand.
	KindACP Kind = "acp"
	// KindVSCodeAgent is a `contenox vscode-agent` stdio process — the VS Code
	// extension's agent. The kind alone identifies the client here.
	KindVSCodeAgent Kind = "vscode-agent"
	// KindServe is the `contenox serve` process itself, registered for symmetry so
	// the board shows serve alongside the editors sharing its store.
	KindServe Kind = "serve"
)

// DefaultTTL is the staleness threshold: a record whose LastSeen is older than
// this is reported Stale. It is "the TTL" for presence liveness. Mirrors the
// modeld owner lease's TTL role.
const DefaultTTL = 30 * time.Second

// DefaultHeartbeatInterval is how often a Reporter renews its record. TTL/3, the
// same renew-to-TTL ratio modeld's owner uses, so two renewals may be missed
// before a live instance is ever reported stale.
const DefaultHeartbeatInterval = DefaultTTL / 3

// DefaultInitialDelay defers a Reporter's FIRST registration write past the
// process's boot-critical window: schema/preset embedding writes the same
// shared SQLite file at startup, and an eager presence write intermittently
// starved that init into "database is locked" on fresh serve boots. Long
// enough to clear the embed phase, short enough that an instance appears on
// the board effectively immediately at human timescales.
const DefaultInitialDelay = 1500 * time.Millisecond

// ReapMultiple sets how long past its TTL a record survives in the store before
// the KV row's own TTL ages it out of the listing entirely: a dead editor shows
// as Stale for (ReapMultiple-1)×TTL — long enough for an operator to SEE it just
// died — and then disappears. N×TTL, per the slice's staleness-honesty rule.
const ReapMultiple = 3

// Record is one instance's presence — the durable-ish fact written to the shared
// store and renewed on each heartbeat. It is deliberately small: identity, kind,
// liveness timestamps, and a few observed facts the board shows. It carries NO
// control surface — see the package doc.
type Record struct {
	// InstanceID is a stable per-process id (a uuid minted at start), the record's
	// key discriminator within its kind.
	InstanceID string `json:"instanceId"`
	// Kind is which contenox surface this process is (acp / vscode-agent / serve).
	Kind Kind `json:"kind"`
	// PID is the OS process id — informational, for an operator correlating the
	// board with `ps`. Presence is enforced by TTL, never by a process check.
	PID int `json:"pid"`
	// Host is the machine hostname — informational, and it keeps records from two
	// hosts sharing a store (an unusual but possible NFS-home setup) legible.
	Host string `json:"host,omitempty"`
	// StartedAt is when the process registered; StartedAt is stable across
	// heartbeats.
	StartedAt time.Time `json:"startedAt"`
	// LastSeen is the last successful heartbeat — the raw liveness fact the board
	// surfaces alongside the derived Stale flag. Bumped on every renew.
	LastSeen time.Time `json:"lastSeen"`
	// Cwd is the process working directory (an editor's project dir), optional.
	Cwd string `json:"cwd,omitempty"`
	// Address is the reachable listen address (host:port) of a serve process —
	// set ONLY for KindServe. It is how a SIBLING process (a standalone `contenox
	// acp` forwarding `/mission`) discovers where a running serve answers without
	// a hardcoded port, over this same shared store. Empty for the editor kinds
	// (acp / vscode-agent), which are stdio processes, not reachable services. It
	// carries NO credential: the bearer token is deliberately NEVER written to
	// this world-readable store (a sibling reads the token from its own
	// CONTENOX_SERVER_TOKEN env instead — see runtime/contenoxcli mission
	// forwarding).
	Address string `json:"address,omitempty"`
	// SessionCount is how many ACP sessions are currently open on the instance —
	// best-effort, updated on session events where the surface exposes them.
	SessionCount int `json:"sessionCount"`
	// ClientName is the editor that spawned the process, from the ACP initialize
	// handshake (Zed identifies itself as "zed"). Empty when the surface does not
	// carry a client identity or none was sent.
	ClientName string `json:"clientName,omitempty"`
}

// Entry is the READ shape the board consumes: a Record plus the two facts the
// reader derives — that it is External (observed, not kernel-managed) and whether
// it is Stale (its TTL lapsed). The wire shape is Record's fields flattened, with
// external and stale appended.
type Entry struct {
	Record
	// External is always true for a presence entry: the kernel owns no lifecycle
	// over it, so the board must not offer Stop/Cancel. It is written explicitly
	// (rather than left implicit) so a board consumer keys "observed, not managed"
	// off a field, not off which endpoint it happened to call.
	External bool `json:"external"`
	// Stale reports that LastSeen is older than the store's staleness TTL — the
	// instance has likely died and is only still listed because it has not yet
	// aged out. Raw LastSeen is surfaced too, so the board can show "how stale".
	Stale bool `json:"stale"`
}

const keyPrefix = "presence:"

func recordKey(kind Kind, instanceID string) string {
	return keyPrefix + string(kind) + ":" + instanceID
}

// Store reads and writes presence records over a shared KV store. It is the one
// place the key layout, the row TTL, and the staleness derivation live, so a
// writer and the board reader can never disagree about them.
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

// Register writes (or renews) rec with the store's row TTL. Renewing is just
// writing again: the same key with a fresh LastSeen and a fresh TTL, so a live
// instance's row never expires while a dead one's ages out. It stamps LastSeen
// when the caller left it zero. Returns an error the caller MAY log; a Reporter
// swallows it (presence is best-effort).
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

// List returns every live presence record, each annotated External and with
// Stale derived from LastSeen against the staleness TTL. Records whose KV row TTL
// has lapsed are already absent (the store filters and lazily deletes expired
// rows), so aging-out needs no reaper here — a stale-but-not-yet-aged record is
// still returned, marked Stale, which is exactly the honesty the board wants.
// Corrupt or vanished-mid-read rows are skipped rather than failing the whole
// board. The result is sorted for a deterministic listing.
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

// Deregister removes a record — the best-effort clean-shutdown path so a row
// vanishes promptly instead of lingering stale until it ages out. Deleting a key
// that is already gone is a no-op.
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
