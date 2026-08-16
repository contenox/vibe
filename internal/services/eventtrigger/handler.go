package eventtrigger

import (
	"context"

	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// Handler is the in-process trigger dispatch: a host that runs an engine mounts
// it on the event append seam so matching triggers fire live in the appending
// process. It shares firingCore with the Dispatcher and writes the same
// event_firings claims, so neither double-fires a (trigger, nid) pair.
type Handler struct {
	firingCore
}

// NewHandler validates deps and builds the in-process handler. Deps.Log,
// Poll, and Batch are unused (live-only, no drain).
func NewHandler(deps Deps) (*Handler, error) {
	core, err := newFiringCore(deps)
	if err != nil {
		return nil, err
	}
	return &Handler{firingCore: core}, nil
}

// HandleEvent fires every trigger matching each event, claim-first through the
// shared firings table. Events of another workspace are skipped, and no error
// propagates.
func (h *Handler) HandleEvent(ctx context.Context, events ...*runtimetypes.Event) {
	for _, ev := range events {
		if ev == nil || ev.WorkspaceID != h.deps.WorkspaceID {
			continue
		}
		h.handle(ctx, *ev)
	}
}

var _ eventlog.Trigger = (*Handler)(nil)
