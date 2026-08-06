package eventtrigger

import (
	"context"

	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// Handler is the in-process trigger dispatch — bob2's eventdispatch handler
// restored over this package's file-defined triggers: a host that runs an
// engine mounts it on the event append seam (eventlog.WithTrigger /
// WithPublisherTrigger), so matching triggers fire live in the appending
// process instead of waiting for a standalone `events dispatch` run.
//
// It shares firingCore with the Dispatcher and writes the SAME event_firings
// claims, so the standalone dispatcher — demoted to catch-up duty for events
// appended while no host ran — never double-fires a (trigger, nid) pair. No
// cursor is kept: the in-process path is live-only, the cursor stays the
// dispatcher's.
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

// HandleEvent implements eventlog.Trigger: fire every trigger matching each
// event, claim-first through the shared firings table. Events of another
// workspace are skipped — one workspace's triggers never fire on another's
// events, the dispatcher's own invariant. Errors are recorded on the firing
// row and via the tracker; none propagate (the append already succeeded).
func (h *Handler) HandleEvent(ctx context.Context, events ...*runtimetypes.Event) {
	for _, ev := range events {
		if ev == nil || ev.WorkspaceID != h.deps.WorkspaceID {
			continue
		}
		h.handle(ctx, *ev)
	}
}

var _ eventlog.Trigger = (*Handler)(nil)
