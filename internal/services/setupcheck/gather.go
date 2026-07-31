package setupcheck

import (
	"context"

	"github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// GatherInput builds Input from SQLite KV defaults, registered backend count, and a runtime state snapshot.
// workspaceID scopes workspace-scoped keys (default-chain, hitl-policy-name) with global fallback.
func GatherInput(ctx context.Context, db libdbexec.DBManager, states []runtimestate.BackendRuntimeState, workspaceID string) (Input, error) {
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
	defaultChain, chainFrom := clikv.ReadConfig(ctx, store, workspaceID, "default-chain")
	hitlPolicy, policyFrom := clikv.ReadConfig(ctx, store, workspaceID, "hitl-policy-name")
	return Input{
		DefaultModel:           clikv.Read(ctx, store, "default-model"),
		DefaultProvider:        clikv.Read(ctx, store, "default-provider"),
		DefaultAltModel:        clikv.Read(ctx, store, "default-alt-model"),
		DefaultAltProvider:     clikv.Read(ctx, store, "default-alt-provider"),
		DefaultEmbedModel:      clikv.Read(ctx, store, "default-embed-model"),
		DefaultEmbedProvider:   clikv.Read(ctx, store, "default-embed-provider"),
		DefaultChain:           defaultChain,
		HITLPolicyName:         hitlPolicy,
		States:                 states,
		RegisteredBackendCount: &n,
		RegisteredBackends:     registered,
		ResolvedFrom: map[string]string{
			"defaultChain":   chainFrom,
			"hitlPolicyName": policyFrom,
		},
	}, nil
}

// StatesFromMap flattens runtime state snapshots for Evaluate / GatherInput.
func StatesFromMap(m map[string]runtimestate.BackendRuntimeState) []runtimestate.BackendRuntimeState {
	if len(m) == 0 {
		return nil
	}
	out := make([]runtimestate.BackendRuntimeState, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
