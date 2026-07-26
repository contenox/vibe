package presence_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libkvstore"
	"github.com/contenox/beam/internal/services/presence"
)

// openKV opens a fresh file-backed SQLite KV manager. A FILE (not :memory:) is
// used so a second manager over the same path exercises the real cross-process
// path — an editor writer and serve's reader are separate processes sharing
// $HOME/.contenox/local.db.
func openKV(t *testing.T, dir string) libkvstore.KVManager {
	t.Helper()
	path := filepath.Join(dir, "local.db")
	db, err := libdbexec.NewSQLiteDBManager(context.Background(), path, libkvstore.SQLiteSchema)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return libkvstore.NewSQLiteManager(db)
}

func TestUnit_Store_RegisterListRoundTrip(t *testing.T) {
	ctx := context.Background()
	kv := openKV(t, t.TempDir())
	store := presence.NewStore(kv)

	rec := presence.Record{
		InstanceID:   "acp-1",
		Kind:         presence.KindACP,
		PID:          4242,
		Host:         "workstation",
		Cwd:          "/home/dev/project",
		SessionCount: 2,
		ClientName:   "zed",
		StartedAt:    time.Now().UTC(),
	}
	if err := store.Register(ctx, rec); err != nil {
		t.Fatalf("Register: %v", err)
	}

	entries, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.InstanceID != "acp-1" || got.Kind != presence.KindACP {
		t.Errorf("identity not preserved: %+v", got)
	}
	if got.PID != 4242 || got.Host != "workstation" || got.Cwd != "/home/dev/project" {
		t.Errorf("observed facts not preserved: %+v", got)
	}
	if got.SessionCount != 2 || got.ClientName != "zed" {
		t.Errorf("session/client not preserved: %+v", got)
	}
	if !got.External {
		t.Error("presence entry must be marked External (observed, not managed)")
	}
	if got.Stale {
		t.Error("a just-registered entry must not be stale")
	}
	if got.LastSeen.IsZero() {
		t.Error("LastSeen must be stamped on Register")
	}
}

func TestUnit_Store_StaleDerivation(t *testing.T) {
	ctx := context.Background()
	kv := openKV(t, t.TempDir())

	// Drive the store clock so staleness is deterministic. The KV row's own hard
	// TTL is left at the default (90s) so the row stays ALIVE in real time while
	// we advance the logical clock past the staleness threshold — this isolates
	// staleness-derivation from aging-out.
	base := time.Now().UTC()
	clock := base
	store := presence.NewStore(kv,
		presence.WithClock(func() time.Time { return clock }),
		presence.WithStaleTTL(30*time.Second),
	)

	if err := store.Register(ctx, presence.Record{InstanceID: "acp-1", Kind: presence.KindACP}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Just after registration: fresh.
	clock = base.Add(5 * time.Second)
	entries, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Stale {
		t.Fatalf("within TTL the entry must be fresh: %+v", entries)
	}

	// Past the TTL since LastSeen: stale, but still listed (not yet aged out) so
	// an operator can SEE it died.
	clock = base.Add(31 * time.Second)
	entries, err = store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || !entries[0].Stale {
		t.Fatalf("past TTL the entry must be listed AND stale: %+v", entries)
	}
	if entries[0].LastSeen.IsZero() {
		t.Error("raw LastSeen must still be surfaced on a stale entry")
	}
}

func TestUnit_Store_AgesOutAfterHardTTL(t *testing.T) {
	ctx := context.Background()
	kv := openKV(t, t.TempDir())

	// A short hard TTL drives real aging-out via the KV row's native expiry — the
	// "filter-on-read" reap: a dead editor's row disappears from the listing with
	// no reaper, because the store treats an expired row as absent.
	store := presence.NewStore(kv,
		presence.WithStaleTTL(10*time.Millisecond),
		presence.WithHardTTL(40*time.Millisecond),
	)
	if err := store.Register(ctx, presence.Record{InstanceID: "acp-1", Kind: presence.KindACP}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if entries, err := store.List(ctx); err != nil || len(entries) != 1 {
		t.Fatalf("registered entry must be listed: entries=%v err=%v", entries, err)
	}

	time.Sleep(80 * time.Millisecond) // past the hard TTL

	entries, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("aged-out entry must be gone from the listing, got %+v", entries)
	}
}

func TestUnit_Store_Deregister(t *testing.T) {
	ctx := context.Background()
	kv := openKV(t, t.TempDir())
	store := presence.NewStore(kv)

	if err := store.Register(ctx, presence.Record{InstanceID: "serve-1", Kind: presence.KindServe}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store.Deregister(ctx, presence.KindServe, "serve-1"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	entries, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("deregistered entry must be gone, got %+v", entries)
	}
	// Deregistering an already-absent key is a no-op, not an error.
	if err := store.Deregister(ctx, presence.KindServe, "serve-1"); err != nil {
		t.Fatalf("idempotent Deregister: %v", err)
	}
}

func TestUnit_Store_RegisterRejectsIncompleteRecord(t *testing.T) {
	ctx := context.Background()
	store := presence.NewStore(openKV(t, t.TempDir()))
	if err := store.Register(ctx, presence.Record{Kind: presence.KindACP}); err == nil {
		t.Error("Register must reject a record with no instance id")
	}
	if err := store.Register(ctx, presence.Record{InstanceID: "x"}); err == nil {
		t.Error("Register must reject a record with no kind")
	}
}

// TestUnit_Board_ComposedFleetView is the slice's composed case: two fresh
// "editor" registrations plus one that has gone stale past its hard TTL. The
// board reader (a SEPARATE KV manager over the same db file, the real
// cross-process shape) must show the two fresh ones marked External, and the
// stale one aged out of the listing.
func TestUnit_Board_ComposedFleetView(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Writers and the reader are distinct managers over the SAME db file.
	writerKV := openKV(t, dir)
	readerKV := openKV(t, dir)

	// The dead editor writes with a short hard TTL so its row genuinely ages out.
	deadWriter := presence.NewStore(writerKV, presence.WithHardTTL(40*time.Millisecond))
	if err := deadWriter.Register(ctx, presence.Record{InstanceID: "dead-zed", Kind: presence.KindACP, ClientName: "zed"}); err != nil {
		t.Fatalf("register dead editor: %v", err)
	}
	time.Sleep(80 * time.Millisecond) // let the dead editor age out

	// Two live editors register with the default (long) hard TTL.
	liveWriter := presence.NewStore(writerKV)
	if err := liveWriter.Register(ctx, presence.Record{InstanceID: "live-zed", Kind: presence.KindACP, ClientName: "zed"}); err != nil {
		t.Fatalf("register live editor: %v", err)
	}
	if err := liveWriter.Register(ctx, presence.Record{InstanceID: "live-code", Kind: presence.KindVSCodeAgent}); err != nil {
		t.Fatalf("register vscode editor: %v", err)
	}

	reader := presence.NewStore(readerKV)
	entries, err := reader.List(ctx)
	if err != nil {
		t.Fatalf("board List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("board must show 2 live editors and drop the aged-out one, got %d: %+v", len(entries), entries)
	}
	ids := map[string]presence.Entry{}
	for _, e := range entries {
		ids[e.InstanceID] = e
		if !e.External {
			t.Errorf("board entry %q must be marked External", e.InstanceID)
		}
		if e.Stale {
			t.Errorf("live board entry %q must not be stale", e.InstanceID)
		}
	}
	if _, ok := ids["dead-zed"]; ok {
		t.Error("the aged-out editor must NOT appear on the board")
	}
	if _, ok := ids["live-zed"]; !ok {
		t.Error("the live acp editor must appear on the board")
	}
	if _, ok := ids["live-code"]; !ok {
		t.Error("the live vscode editor must appear on the board")
	}
}
