// backends.go contains helpers for LLM backend and provider config KV storage.
package contenoxcli

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/contenox/contenox/runtime/internal/runtimestate"
	"github.com/contenox/contenox/runtime/runtimetypes"
)

// setProviderConfigKV stores a cloud provider API key in the SQLite KV store.
// Used by backend_cmd.go when a backend with an API key is registered.
func setProviderConfigKV(ctx context.Context, store runtimetypes.Store, providerType, apiKey string) error {
	key := runtimestate.ProviderKeyPrefix + strings.ToLower(providerType)
	pc := runtimestate.ProviderConfig{APIKey: apiKey, Type: providerType}
	data, err := json.Marshal(pc)
	if err != nil {
		return nil // Non-fatal for backward compat.
	}
	return store.SetKV(ctx, key, json.RawMessage(data))
}
