package libkvstore_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libkvstore"
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

func TestUnit_SQLiteKVSetGet(t *testing.T) {
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

func TestUnit_SQLiteKVNotFound(t *testing.T) {
	exec := openSQLiteKV(t)
	_, err := exec.Get(context.Background(), "nonexistent")
	if err != libkvstore.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestUnit_SQLiteKVTTLExpiry(t *testing.T) {
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

func TestUnit_SQLiteKVExists(t *testing.T) {
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

func TestUnit_SQLiteKVDelete(t *testing.T) {
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

func TestUnit_SQLiteKVKeys(t *testing.T) {
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

func TestUnit_SQLiteKVListPushRange(t *testing.T) {
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

func TestUnit_SQLiteKVSetAddMembers(t *testing.T) {
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

func TestUnit_SQLiteKVOverwrite(t *testing.T) {
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

// openSQLiteKVFile opens a file-backed SQLite KV store. Concurrency tests must not
// use ":memory:" because every pooled connection would get its own private database.
func openSQLiteKVFile(t *testing.T) libkvstore.KVExecutor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kv.db")
	db, err := libdbexec.NewSQLiteDBManager(context.Background(), path, libkvstore.SQLiteSchema)
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

func TestUnit_SQLiteKVConcurrentListPushKeepsEveryElement(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKVFile(t)

	const n = 32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := exec.ListPush(ctx, "concurrent", json.RawMessage(strconv.Itoa(i))); err != nil {
				t.Errorf("ListPush(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	items, err := exec.ListRange(ctx, "concurrent", 0, -1)
	if err != nil {
		t.Fatalf("ListRange: %v", err)
	}
	if len(items) != n {
		t.Fatalf("lost updates: expected %d items, got %d", n, len(items))
	}
	seen := map[int]bool{}
	for _, it := range items {
		var v int
		if err := json.Unmarshal(it, &v); err != nil {
			t.Fatalf("unmarshal %q: %v", it, err)
		}
		seen[v] = true
	}
	for i := range n {
		if !seen[i] {
			t.Errorf("element %d was lost", i)
		}
	}
}

func TestUnit_SQLiteKVConcurrentSetAddKeepsEveryMember(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKVFile(t)

	const n = 32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := exec.SetAdd(ctx, "concurrentset", json.RawMessage(strconv.Itoa(i))); err != nil {
				t.Errorf("SetAdd(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	members, err := exec.SetMembers(ctx, "concurrentset")
	if err != nil {
		t.Fatalf("SetMembers: %v", err)
	}
	if len(members) != n {
		t.Fatalf("lost updates: expected %d members, got %d", n, len(members))
	}
}

func TestUnit_SQLiteKVKeysWithLikeSpecialCharacters(t *testing.T) {
	ctx := context.Background()
	exec := openSQLiteKV(t)

	all := []string{`a%b`, `a_b`, `axb`, `a\b`, `plain`}
	for _, k := range all {
		if err := exec.Set(ctx, k, json.RawMessage(`1`)); err != nil {
			t.Fatalf("Set(%q): %v", k, err)
		}
	}

	cases := []struct {
		pattern string
		want    []string
	}{
		{`a%b`, []string{`a%b`}},
		{`a_b`, []string{`a_b`}},
		{`a\b`, []string{`a\b`}},
		{`a*`, []string{`a%b`, `a_b`, `axb`, `a\b`}},
		{`*`, all},
		{`plain`, []string{`plain`}},
	}
	for _, tc := range cases {
		keys, err := exec.Keys(ctx, tc.pattern)
		if err != nil {
			t.Fatalf("Keys(%q): %v", tc.pattern, err)
		}
		got := make([]string, len(keys))
		copy(got, keys)
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("Keys(%q) = %v, want %v", tc.pattern, got, want)
		}
	}
}
