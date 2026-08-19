package searchtool

import (
	"context"
	"fmt"
	"sync"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/workspaceindex"
)

// DeferredTools is the two-stage registration this toolset needs: the toolset
// map is built before the engine that owns the embedding seam exists, so the
// repo is registered unbound and Bind completes it afterwards. Until then the
// tool answers with the no-index note rather than failing, so a session that
// never builds an engine still renders.
type DeferredTools struct {
	taskengine.ToolsRepo
	q *deferredQuerier
}

// NewDeferredTools registers unbound under ToolsProviderName; workspaceID is
// fixed here because a model naming its own would read another project's index.
func NewDeferredTools(workspaceID string) *DeferredTools {
	q := &deferredQuerier{}
	return &DeferredTools{ToolsRepo: NewTools(q, workspaceID), q: q}
}

// Bind is safe to call while the repo is already serving; a nil querier is a
// no-op, not an unbinding.
func (d *DeferredTools) Bind(q Querier) {
	if d == nil || q == nil {
		return
	}
	d.q.bind(q)
}

func (d *DeferredTools) Bound() bool {
	return d != nil && d.q.bound()
}

type deferredQuerier struct {
	mu  sync.RWMutex
	svc Querier
}

func (d *deferredQuerier) bind(svc Querier) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.svc = svc
}

func (d *deferredQuerier) bound() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.svc != nil
}

// Query wraps ErrNoIndex while unbound so the toolset answers with the runnable
// instruction it gives an unindexed workspace, not with a fault the model would
// read as its own mistake.
func (d *deferredQuerier) Query(ctx context.Context, workspaceID, question string, topK int) ([]workspaceindex.Hit, error) {
	d.mu.RLock()
	svc := d.svc
	d.mu.RUnlock()
	if svc == nil {
		return nil, fmt.Errorf("%w: the index is not attached to this session", workspaceindex.ErrNoIndex)
	}
	return svc.Query(ctx, workspaceID, question, topK)
}

var _ Querier = (*deferredQuerier)(nil)
