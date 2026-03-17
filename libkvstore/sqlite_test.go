package libkvstore_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
)

func openSQLiteKV(t *testing.T) libkvstore.KVExecutor {
	t.Helper()
	db, err := libdbexec.NewSQLiteDBManager(context.Background(), ":memory:", libkvstore.SQLiteSchema)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mgr := libkvstore.NewSQLiteManager(db)
	exec, err := mgr.Executor(context.Background())
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	return exec
}

func TestSQLiteKVSetGet(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKV(t)

	payload, _ := json.Marshal("hello world")
	if err := exec.Set(ctx, "k1", payload); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := exec.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var val string
	json.Unmarshal(got, &val)
	if val != "hello world" {
		t.Errorf("want 'hello world', got %q", val)
	}
}

func TestSQLiteKVNotFound(t *testing.T) {
	exec := openSQLiteKV(t)
	_, err := exec.Get(context.Background(), "nonexistent")
	if err != libkvstore.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestSQLiteKVTTLExpiry(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKV(t)

	payload, _ := json.Marshal(42)
	if err := exec.SetWithTTL(ctx, "expkey", payload, 50*time.Millisecond); err != nil {
		t.Fatalf("SetWithTTL: %v", err)
	}

	// Should be available immediately
	got, err := exec.Get(ctx, "expkey")
	if err != nil {
		t.Fatalf("Get before expiry: %v", err)
	}
	var n int
	json.Unmarshal(got, &n)
	if n != 42 {
		t.Errorf("want 42, got %d", n)
	}

	// Wait past TTL
	time.Sleep(100 * time.Millisecond)
	_, err = exec.Get(ctx, "expkey")
	if err != libkvstore.ErrNotFound {
		t.Errorf("expected ErrNotFound after TTL, got %v", err)
	}
}

func TestSQLiteKVExists(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKV(t)

	ok, err := exec.Exists(ctx, "x")
	if err != nil || ok {
		t.Errorf("expected (false, nil), got (%v, %v)", ok, err)
	}

	exec.Set(ctx, "x", json.RawMessage(`1`))
	ok, err = exec.Exists(ctx, "x")
	if err != nil || !ok {
		t.Errorf("expected (true, nil), got (%v, %v)", ok, err)
	}
}

func TestSQLiteKVDelete(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKV(t)

	exec.Set(ctx, "del", json.RawMessage(`"bye"`))
	if err := exec.Delete(ctx, "del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := exec.Get(ctx, "del")
	if err != libkvstore.ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestSQLiteKVKeys(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKV(t)

	for _, k := range []string{"prov:a", "prov:b", "other"} {
		exec.Set(ctx, k, json.RawMessage(`1`))
	}
	keys, err := exec.Keys(ctx, "prov:*")
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("expected 2 keys matching prov:*, got %d: %v", len(keys), keys)
	}
}

func TestSQLiteKVListPushRange(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKV(t)

	for _, v := range []string{"a", "b", "c"} {
		exec.ListPush(ctx, "mylist", json.RawMessage(`"`+v+`"`))
	}
	items, err := exec.ListRange(ctx, "mylist", 0, -1)
	if err != nil {
		t.Fatalf("ListRange: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("expected 3 items, got %d", len(items))
	}
	// LPUSH means newest is index 0
	var first string
	json.Unmarshal(items[0], &first)
	if first != "c" {
		t.Errorf("expected 'c' at index 0 (LPUSH), got %q", first)
	}
}

func TestSQLiteKVSetAddMembers(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKV(t)

	exec.SetAdd(ctx, "s", json.RawMessage(`"x"`))
	exec.SetAdd(ctx, "s", json.RawMessage(`"y"`))
	exec.SetAdd(ctx, "s", json.RawMessage(`"x"`)) // duplicate

	members, err := exec.SetMembers(ctx, "s")
	if err != nil {
		t.Fatalf("SetMembers: %v", err)
	}
	if len(members) != 2 {
		t.Errorf("expected 2 unique members, got %d", len(members))
	}
}

func TestSQLiteKVOverwrite(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKV(t)

	exec.Set(ctx, "ow", json.RawMessage(`"first"`))
	exec.Set(ctx, "ow", json.RawMessage(`"second"`))
	got, _ := exec.Get(ctx, "ow")
	var val string
	json.Unmarshal(got, &val)
	if val != "second" {
		t.Errorf("expected 'second' after overwrite, got %q", val)
	}
}
