package searchtool

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/workspaceindex"
)

func execDeferred(t *testing.T, repo taskengine.ToolsRepo) *Result {
	t.Helper()
	out, dt, err := repo.Exec(context.Background(), time.Now(), map[string]any{"question": "where is retry backoff"}, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolSearch})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if dt != taskengine.DataTypeJSON {
		t.Errorf("data type = %v, want JSON", dt)
	}
	res, ok := out.(*Result)
	if !ok {
		t.Fatalf("result is %T, want *Result", out)
	}
	return res
}

// An unbound repo is the state the toolset map is in between construction and
// the engine being built. It must answer, not fail: the alternative is a model
// that reads a wiring gap as its own bad call and retries it.
func TestUnit_Deferred_UnboundAnswersWithTheNoIndexNote(t *testing.T) {
	repo := NewDeferredTools(testWorkspaceID)
	if repo.Bound() {
		t.Fatal("a fresh repo must not report itself bound")
	}
	res := execDeferred(t, repo)
	if len(res.Hits) != 0 {
		t.Errorf("%d hits from an unbound repo", len(res.Hits))
	}
	if !strings.Contains(res.Note, "contenox index") {
		t.Errorf("note must name the command that fixes it: %q", res.Note)
	}
	if !strings.Contains(res.Note, "not a failure") {
		t.Errorf("note must say this is not a fault of the tool: %q", res.Note)
	}
}

func TestUnit_Deferred_BindCompletesTheRepoInPlace(t *testing.T) {
	repo := NewDeferredTools(testWorkspaceID)
	q := &fakeQuerier{hits: []workspaceindex.Hit{hit("docs/retry.md", 10, 24, "backoff doubles")}}
	repo.Bind(q)

	if !repo.Bound() {
		t.Fatal("Bind did not take")
	}
	res := execDeferred(t, repo)
	if len(res.Hits) != 1 || res.Hits[0].Citation != "docs/retry.md:10-24" {
		t.Fatalf("%+v", res.Hits)
	}
	// The workspace is still the one fixed at construction, never the call's.
	if q.gotWorkspace != testWorkspaceID {
		t.Errorf("workspace = %q, want %q", q.gotWorkspace, testWorkspaceID)
	}
	// A nil bind is a no-op rather than an unbinding.
	repo.Bind(nil)
	if !repo.Bound() {
		t.Error("a nil Bind unbound a live repo")
	}
}

// inertQuerier answers without recording, so the only shared state under -race
// is the bind seam itself.
type inertQuerier struct{}

func (inertQuerier) Query(context.Context, string, string, int) ([]workspaceindex.Hit, error) {
	return nil, nil
}

// Bind happens on the wiring goroutine while the repo may already be serving,
// so the seam is read under a lock. Run with -race.
func TestUnit_Deferred_BindIsSafeAgainstConcurrentCalls(t *testing.T) {
	repo := NewDeferredTools(testWorkspaceID)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = repo.Exec(context.Background(), time.Now(), map[string]any{"question": "x"}, false,
				&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolSearch})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		repo.Bind(inertQuerier{})
	}()
	wg.Wait()
}

// The deferred wrapper must stay a ToolsRepo in every respect, including the
// name the gate and the policy key on.
func TestUnit_Deferred_KeepsTheToolsetContract(t *testing.T) {
	var repo taskengine.ToolsRepo = NewDeferredTools(testWorkspaceID)
	got, err := repo.Supports(context.Background())
	if err != nil {
		t.Fatalf("Supports: %v", err)
	}
	if strings.Join(got, ",") != strings.Join([]string{ToolsProviderName, ToolSearch}, ",") {
		t.Fatalf("Supports() = %v", got)
	}
	if _, err := repo.GetToolsForToolsByName(context.Background(), ToolsProviderName); err != nil {
		t.Errorf("GetToolsForToolsByName: %v", err)
	}
	if _, err := repo.GetSchemasForSupportedTools(context.Background()); err != nil {
		t.Errorf("GetSchemasForSupportedTools: %v", err)
	}
}
