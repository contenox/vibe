// backends.go ensures backends from config exist in the DB and cloud provider API keys in KV.
package vibecli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/internal/runtimestate"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/google/uuid"
)

// ensureBackendsFromConfig ensures all resolved backends exist in the DB and cloud provider API keys are stored in KV.
func ensureBackendsFromConfig(ctx context.Context, db libdb.DBManager, backendSvc backendservice.Service, resolved []resolvedBackend) error {
	tx := db.WithoutTransaction()
	store := runtimetypes.New(tx)
	list, err := backendSvc.List(ctx, nil, 100)
	if err != nil {
		return err
	}
	byName := make(map[string]*runtimetypes.Backend)
	for _, b := range list {
		byName[b.Name] = b
	}
	for _, r := range resolved {
		if r.baseURL == "" {
			continue
		}
		if existing, ok := byName[r.name]; ok {
			if existing.BaseURL != r.baseURL || existing.Type != r.typ {
				existing.BaseURL = r.baseURL
				existing.Type = r.typ
				if err := backendSvc.Update(ctx, existing); err != nil {
					return fmt.Errorf("update backend %s: %w", r.name, err)
				}
			}
			if r.typ == "openai" || r.typ == "gemini" {
				if err := setProviderConfigKV(ctx, store, r.typ, r.apiKey); err != nil {
					return err
				}
			}
			continue
		}
		backend := &runtimetypes.Backend{
			ID:      uuid.NewString(),
			Name:    r.name,
			BaseURL: r.baseURL,
			Type:    r.typ,
		}
		if err := backendSvc.Create(ctx, backend); err != nil {
			return fmt.Errorf("create backend %s: %w", r.name, err)
		}
		byName[r.name] = backend
		if r.typ == "openai" || r.typ == "gemini" {
			if err := setProviderConfigKV(ctx, store, r.typ, r.apiKey); err != nil {
				return err
			}
		}
	}
	return nil
}

func setProviderConfigKV(ctx context.Context, store runtimetypes.Store, providerType, apiKey string) error {
	key := runtimestate.ProviderKeyPrefix + strings.ToLower(providerType)
	pc := runtimestate.ProviderConfig{APIKey: apiKey, Type: providerType}
	data, err := json.Marshal(pc)
	if err != nil {
		return fmt.Errorf("marshal provider config: %w", err)
	}
	return store.SetKV(ctx, key, json.RawMessage(data))
}
