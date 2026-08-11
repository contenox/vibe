package libevents_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libevents"
)

var testCfg = libevents.Config{TablePrefix: "event_", ScopeColumn: "workspace_id"}

func newDB(t *testing.T) libdb.DBManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "test.db"), "")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := libevents.InitSchema(context.Background(), db.WithoutTransaction(), testCfg); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	return db
}

func TestUnit_Config_RejectsUnsafeIdentifiers(t *testing.T) {
	for _, cfg := range []libevents.Config{
		{TablePrefix: "evil; DROP TABLE x;--", ScopeColumn: "workspace_id"},
		{TablePrefix: "event_", ScopeColumn: `ws" OR 1=1`},
		{TablePrefix: "", ScopeColumn: "workspace_id"},
		{TablePrefix: "Event_", ScopeColumn: "workspace_id"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("config %+v passed validation", cfg)
		}
	}
}

func TestUnit_Cursors_GetSetRewindAndScopeIsolation(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	exec := db.WithoutTransaction()
	a, err := libevents.NewCursorStore(testCfg, "ws-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := libevents.NewCursorStore(testCfg, "ws-b")
	if err != nil {
		t.Fatal(err)
	}

	nid, err := a.GetCursor(ctx, exec, "mail")
	if err != nil || nid != 0 {
		t.Fatalf("fresh cursor = %d, %v; want 0, nil", nid, err)
	}
	if err := a.SetCursor(ctx, exec, "mail", 42); err != nil {
		t.Fatal(err)
	}
	if err := a.SetCursor(ctx, exec, "mail", 99); err != nil {
		t.Fatal(err)
	}
	if nid, _ = a.GetCursor(ctx, exec, "mail"); nid != 99 {
		t.Fatalf("cursor after upsert = %d; want 99", nid)
	}
	if err := a.SetCursor(ctx, exec, "mail", 10); err != nil {
		t.Fatalf("rewind refused: %v", err)
	}
	if nid, _ = a.GetCursor(ctx, exec, "mail"); nid != 10 {
		t.Fatalf("cursor after rewind = %d; want 10", nid)
	}
	if nid, _ = b.GetCursor(ctx, exec, "mail"); nid != 0 {
		t.Fatalf("scope b sees scope a's cursor: %d", nid)
	}
}

func TestUnit_Firings_ClaimIsPrimaryKeyGuarantee(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	exec := db.WithoutTransaction()
	s, err := libevents.NewFiringStore(testCfg, "ws-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	won, err := s.BeginFiring(ctx, exec, "trig", 7, "req-1")
	if err != nil || !won {
		t.Fatalf("first claim = %v, %v; want true, nil", won, err)
	}
	won, err = s.BeginFiring(ctx, exec, "trig", 7, "req-2")
	if err != nil || won {
		t.Fatalf("second claim = %v, %v; want false, nil", won, err)
	}
	if err := s.FinishFiring(ctx, exec, "trig", 7, libevents.FiringStatusOK, ""); err != nil {
		t.Fatal(err)
	}
	if won, _ = s.BeginFiring(ctx, exec, "trig", 7, "req-3"); won {
		t.Fatal("settled firing was reclaimed")
	}

	other, err := libevents.NewFiringStore(testCfg, "ws-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if won, _ = other.BeginFiring(ctx, exec, "trig", 7, "req-4"); !won {
		t.Fatal("scope b blocked by scope a's claim")
	}
}

func TestUnit_Firings_TransactionalClaimReleasesOnRollback(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	s, err := libevents.NewFiringStore(testCfg, "ws-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	tx, _, release, err := db.WithTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.BeginFiring(ctx, tx, "trig", 1, "req-1"); err != nil || !won {
		t.Fatalf("claim in tx = %v, %v", won, err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}

	won, err := s.BeginFiring(ctx, db.WithoutTransaction(), "trig", 1, "req-2")
	if err != nil || !won {
		t.Fatalf("claim after rollback = %v, %v; want true — the transaction is the lock", won, err)
	}
}

func TestUnit_Firings_StaleClaimTakeoverAndReset(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	exec := db.WithoutTransaction()
	clock := time.Now().UTC()
	now := func() time.Time { return clock }
	s, err := libevents.NewFiringStore(testCfg, "ws-a", 30*time.Minute, libevents.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}

	if won, _ := s.BeginFiring(ctx, exec, "trig", 5, "req-1"); !won {
		t.Fatal("first claim lost")
	}
	clock = clock.Add(10 * time.Minute)
	if won, _ := s.BeginFiring(ctx, exec, "trig", 5, "req-2"); won {
		t.Fatal("live claim stolen inside the bound")
	}
	clock = clock.Add(25 * time.Minute)
	if won, _ := s.BeginFiring(ctx, exec, "trig", 5, "req-3"); !won {
		t.Fatal("stale claim not taken over")
	}

	firings, err := s.ListFirings(ctx, exec, libevents.FiringFilter{})
	if err != nil || len(firings) != 1 {
		t.Fatalf("list = %d firings, %v", len(firings), err)
	}
	if firings[0].RequestID != "req-3" || firings[0].CreatedAt.After(firings[0].UpdatedAt) {
		t.Fatalf("takeover row wrong: %+v", firings[0])
	}

	if err := s.FinishFiring(ctx, exec, "trig", 5, libevents.FiringStatusError, "smtp handshake failed"); err != nil {
		t.Fatal(err)
	}
	reset, err := s.ResetFiring(ctx, exec, "trig", 5)
	if err != nil || !reset {
		t.Fatalf("reset settled firing = %v, %v", reset, err)
	}
	if won, _ := s.BeginFiring(ctx, exec, "trig", 5, "req-4"); !won {
		t.Fatal("reset firing not immediately reclaimable")
	}
	if reset, _ = s.ResetFiring(ctx, exec, "trig", 5); reset {
		t.Fatal("live running claim was reset")
	}
}

func TestUnit_Firings_ListFiltersAndStranded(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	exec := db.WithoutTransaction()
	s, err := libevents.NewFiringStore(testCfg, "ws-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for nid := int64(1); nid <= 3; nid++ {
		if _, err := s.BeginFiring(ctx, exec, "trig", nid, "r"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FinishFiring(ctx, exec, "trig", 2, libevents.FiringStatusError, "boom"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListFirings(ctx, exec, libevents.FiringFilter{Status: libevents.FiringStatusError})
	if err != nil || len(got) != 1 || got[0].NID != 2 || got[0].Error != "boom" {
		t.Fatalf("status filter got %+v, %v", got, err)
	}
	got, err = s.ListFirings(ctx, exec, libevents.FiringFilter{SinceNID: 2})
	if err != nil || len(got) != 1 || got[0].NID != 3 {
		t.Fatalf("since filter got %+v, %v", got, err)
	}
	got, _ = s.ListFirings(ctx, exec, libevents.FiringFilter{})
	if len(got) != 3 || got[0].NID != 3 {
		t.Fatalf("default list not newest-first: %+v", got)
	}

	if got[0].Stranded(time.Now(), s.StaleClaim()) {
		t.Fatal("fresh running claim reported stranded")
	}
	if !got[0].Stranded(time.Now().Add(2*time.Minute), s.StaleClaim()) {
		t.Fatal("aged running claim not reported stranded")
	}
}

func TestUnit_Listeners_CRUDTopicsOwnersPagination(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	exec := db.WithoutTransaction()
	s, err := libevents.NewListenerStore(testCfg, "ws-a")
	if err != nil {
		t.Fatal(err)
	}

	l1 := &libevents.Listener{
		ID: "l1", Kind: libevents.ListenerKindStart, Target: `{"chain":"triage"}`,
		Owner: "consumer-cfg-1", Types: []string{"github.push", "github.pr"},
		ContextFilters: map[string]map[string]string{"github.push": {"repo": "contenox/*"}},
		Metadata:       `{"session":"new"}`,
	}
	if err := s.AppendListener(ctx, exec, l1); err != nil {
		t.Fatal(err)
	}
	l2 := &libevents.Listener{
		ID: "l2", Kind: libevents.ListenerKindWake, Target: "instance-9",
		Owner: "session-9", OneShot: true, Types: []string{"github.pr"},
	}
	if err := s.AppendListener(ctx, exec, l2); err != nil {
		t.Fatal(err)
	}

	if err := s.AppendListener(ctx, exec, &libevents.Listener{ID: "bad", Kind: "explode", Types: []string{"t"}}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if err := s.AppendListener(ctx, exec, &libevents.Listener{ID: "bad2", Kind: libevents.ListenerKindStart}); err == nil {
		t.Fatal("typeless listener accepted")
	}

	got, err := s.GetListener(ctx, exec, "l1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Types) != 2 || got.ContextFilters["github.push"]["repo"] != "contenox/*" || got.Metadata != `{"session":"new"}` {
		t.Fatalf("round trip lost data: %+v", got)
	}

	byType, err := s.ListListenersByType(ctx, exec, "github.pr")
	if err != nil || len(byType) != 2 {
		t.Fatalf("topic lookup = %d, %v; want 2", len(byType), err)
	}
	byType, err = s.ListListenersByType(ctx, exec, "github.push")
	if err != nil || len(byType) != 1 || byType[0].ID != "l1" {
		t.Fatalf("topic lookup push = %+v, %v", byType, err)
	}

	other, _ := libevents.NewListenerStore(testCfg, "ws-b")
	if lst, _ := other.ListListenersByType(ctx, exec, "github.pr"); len(lst) != 0 {
		t.Fatal("scope b sees scope a's listeners")
	}

	page, err := s.ListListeners(ctx, exec, time.Time{}, "", 1)
	if err != nil || len(page) != 1 {
		t.Fatalf("page 1 = %d, %v", len(page), err)
	}
	page2, err := s.ListListeners(ctx, exec, page[0].CreatedAt, page[0].ID, 10)
	if err != nil || len(page2) != 1 || page2[0].ID == page[0].ID {
		t.Fatalf("page 2 = %+v, %v", page2, err)
	}

	removed, err := s.DeleteListenersByOwner(ctx, exec, "session-9")
	if err != nil || len(removed) != 1 || removed[0] != "l2" {
		t.Fatalf("owner cleanup = %v, %v", removed, err)
	}
	if lst, _ := s.ListListenersByType(ctx, exec, "github.pr"); len(lst) != 1 {
		t.Fatal("owner cleanup left topic rows behind")
	}
	if err := s.DeleteListener(ctx, exec, "l2"); err == nil {
		t.Fatal("double delete succeeded")
	}
}

func TestUnit_Listeners_OneShotConsumptionSharesFiringTransaction(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	s, _ := libevents.NewListenerStore(testCfg, "ws-a")
	f, _ := libevents.NewFiringStore(testCfg, "ws-a", time.Minute)
	if err := s.AppendListener(ctx, db.WithoutTransaction(), &libevents.Listener{
		ID: "wake-1", Kind: libevents.ListenerKindWake, OneShot: true, Types: []string{"ci.done"}, Owner: "sess",
	}); err != nil {
		t.Fatal(err)
	}

	tx, _, release, err := db.WithTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if won, _ := f.BeginFiring(ctx, tx, "wake-1", 11, "req"); !won {
		t.Fatal("claim lost")
	}
	if err := s.DeleteListener(ctx, tx, "wake-1"); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}

	exec := db.WithoutTransaction()
	if _, err := s.GetListener(ctx, exec, "wake-1"); err != nil {
		t.Fatalf("rollback lost the listener: %v", err)
	}
	if won, _ := f.BeginFiring(ctx, exec, "wake-1", 11, "req-2"); !won {
		t.Fatal("rollback kept the claim")
	}
}

func TestUnit_Staging_DueOrderingAndTransactionalDrain(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	exec := db.WithoutTransaction()
	s, err := libevents.NewStagingStore(testCfg, "ws-a")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	for _, e := range []*libevents.StagedEvent{
		{ID: "later", Payload: []byte(`{"n":2}`), DelayedUntil: base.Add(time.Hour)},
		{ID: "due-2", Payload: []byte(`{"n":1}`), DelayedUntil: base.Add(-time.Minute)},
		{ID: "due-1", Payload: []byte(`{"n":0}`), DelayedUntil: base.Add(-time.Hour)},
	} {
		if err := s.AppendStagedEvent(ctx, exec, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendStagedEvent(ctx, exec, &libevents.StagedEvent{ID: "no-payload"}); err == nil {
		t.Fatal("payloadless staged event accepted")
	}

	due, err := s.ListDueStagedEvents(ctx, exec, base, 10)
	if err != nil || len(due) != 2 || due[0].ID != "due-1" || due[1].ID != "due-2" {
		t.Fatalf("due list = %+v, %v", due, err)
	}

	tx, _, release, err := db.WithTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStagedEvents(ctx, tx, "due-1", "due-2"); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if due, _ = s.ListDueStagedEvents(ctx, exec, base, 10); len(due) != 2 {
		t.Fatal("rolled-back drain lost staged events")
	}

	if err := s.DeleteStagedEvents(ctx, exec, "due-1", "due-2"); err != nil {
		t.Fatal(err)
	}
	if due, _ = s.ListDueStagedEvents(ctx, exec, base, 10); len(due) != 0 {
		t.Fatalf("drained events still due: %+v", due)
	}
}
