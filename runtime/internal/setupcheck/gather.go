package setupcheck

import (
	"context"

	"github.com/contenox/contenox/runtime/internal/clikv"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/statetype"
)

// GatherInput builds Input from SQLite KV defaults, registered backend count, and a runtime state snapshot.
// workspaceID scopes workspace-scoped keys (default-chain, hitl-policy-name) with global fallback.
func GatherInput(ctx context.Context, db libdbexec.DBManager, states []statetype.BackendRuntimeState, workspaceID string) (Input, error) {
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
