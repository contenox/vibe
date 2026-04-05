package setupcheck

import (
	"context"

	"github.com/contenox/contenox/internal/clikv"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/statetype"
)

// GatherInput builds Input from SQLite KV defaults, registered backend count, and a runtime state snapshot.
// Callers obtain states from stateservice.Get or runtimestate.State.Get (values from the map, in stable order if needed).
func GatherInput(ctx context.Context, db libdbexec.DBManager, states []statetype.BackendRuntimeState) (Input, error) {
	store := runtimetypes.New(db.WithoutTransaction())
	backends, err := store.ListBackends(ctx, nil, runtimetypes.MAXLIMIT)
	if err != nil {
		return Input{}, err
	}
	n := len(backends)
	registered := make([]runtimetypes.Backend, 0, len(backends))
	for _, backend := range backends {
		if backend == nil {
			continue
		}
		registered = append(registered, *backend)
	}
	return Input{
		DefaultModel:           clikv.Read(ctx, store, "default-model"),
		DefaultProvider:        clikv.Read(ctx, store, "default-provider"),
		DefaultChain:           clikv.Read(ctx, store, "default-chain"),
		States:                 states,
		RegisteredBackendCount: &n,
		RegisteredBackends:     registered,
	}, nil
}

// StatesFromMap flattens runtime state snapshots for Evaluate / GatherInput.
func StatesFromMap(m map[string]statetype.BackendRuntimeState) []statetype.BackendRuntimeState {
	if len(m) == 0 {
		return nil
	}
	out := make([]statetype.BackendRuntimeState, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
