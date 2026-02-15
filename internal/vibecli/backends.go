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

// isUniqueConstraintBaseURLError reports whether err is a UNIQUE constraint failure on llm_backends.base_url.
// For the CLI, this is not an error: the backend with that URL already exists.
func isUniqueConstraintBaseURLError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, "base_url")
}

// ensureBackendsFromConfig ensures all resolved backends exist in the DB and cloud provider API keys are stored in KV.
// Existing backends are matched by name; if a backend with the same base_url but different name exists (DB has UNIQUE on base_url), it is updated to match the resolved name/type instead of creating a duplicate.
func ensureBackendsFromConfig(ctx context.Context, db libdb.DBManager, backendSvc backendservice.Service, resolved []resolvedBackend) error {
	tx := db.WithoutTransaction()
	store := runtimetypes.New(tx)
	list, err := backendSvc.List(ctx, nil, 100)
	if err != nil {
		return err
	}
	byName := make(map[string]*runtimetypes.Backend)
	byBaseURL := make(map[string]*runtimetypes.Backend)
	for _, b := range list {
		byName[b.Name] = b
		byBaseURL[b.BaseURL] = b
	}
	for _, r := range resolved {
		if r.baseURL == "" {
			continue
		}
		existing := byName[r.name]
		if existing == nil {
			// Same base_url already stored under a different name (DB has UNIQUE on base_url): update name/type to match config.
			existing = byBaseURL[r.baseURL]
		}
		if existing != nil {
			if existing.Name != r.name || existing.BaseURL != r.baseURL || existing.Type != r.typ {
				delete(byName, existing.Name)
				existing.Name = r.name
				existing.BaseURL = r.baseURL
				existing.Type = r.typ
				if err := backendSvc.Update(ctx, existing); err != nil {
					return fmt.Errorf("update backend %s: %w", r.name, err)
				}
				byName[r.name] = existing
				byBaseURL[r.baseURL] = existing
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
			if isUniqueConstraintBaseURLError(err) {
				// Backend with this base_url already exists; for the CLI this is fine, continue.
				list, err = backendSvc.List(ctx, nil, 100)
				if err != nil {
					return fmt.Errorf("list backends after conflict: %w", err)
				}
				byName = make(map[string]*runtimetypes.Backend)
				byBaseURL = make(map[string]*runtimetypes.Backend)
				for _, b := range list {
					byName[b.Name] = b
					byBaseURL[b.BaseURL] = b
				}
				continue
			}
			return fmt.Errorf("create backend %s: %w", r.name, err)
		}
		byName[r.name] = backend
		byBaseURL[r.baseURL] = backend
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
