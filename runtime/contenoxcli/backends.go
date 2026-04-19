// backends.go contains helpers for LLM backend and provider config KV storage.
package contenoxcli

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/contenox/contenox/runtime/internal/runtimestate"
	"github.com/contenox/contenox/runtime/runtimetypes"
)

func setProviderConfigKV(ctx context.Context, store runtimetypes.Store, providerType, apiKey string) error {
	key := runtimestate.ProviderKeyPrefix + strings.ToLower(providerType)
	pc := runtimestate.ProviderConfig{APIKey: apiKey, Type: providerType}
	data, err := json.Marshal(pc)
	if err != nil {
		return nil // Non-fatal for backward compat.
	}
	return store.SetKV(ctx, key, json.RawMessage(data))
}
