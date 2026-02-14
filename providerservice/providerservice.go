package providerservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/vibe/internal/runtimestate"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
)

const (
	ProviderTypeOpenAI = "openai"
	ProviderTypeGemini = "gemini"
)

type Service interface {
	SetProviderConfig(ctx context.Context, providerType string, upsert bool, config *runtimestate.ProviderConfig) error
	GetProviderConfig(ctx context.Context, providerType string) (*runtimestate.ProviderConfig, error)
	DeleteProviderConfig(ctx context.Context, providerType string) error
	ListProviderConfigs(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimestate.ProviderConfig, error)
}

type service struct {
	dbInstance libdb.DBManager
}

func New(dbInstance libdb.DBManager) Service {
	return &service{dbInstance: dbInstance}
}

func (s *service) SetProviderConfig(ctx context.Context, providerType string, replace bool, config *runtimestate.ProviderConfig) error {
	// Input validation
	if providerType != ProviderTypeOpenAI && providerType != ProviderTypeGemini {
		return fmt.Errorf("invalid provider type: %s", providerType)
	}
	if config == nil {
		return fmt.Errorf("missing config")
	}
	if config.APIKey == "" {
		return fmt.Errorf("missing API key")
	}

	tx, com, r, err := s.dbInstance.WithTransaction(ctx)
	if err != nil {
		return err
	}
	defer r()

	storeInstance := runtimetypes.New(tx)
	count, err := storeInstance.EstimateKVCount(ctx)
	if err != nil {
		return fmt.Errorf("failed to estimate KV count: %w", err)
	}
	err = storeInstance.EnforceMaxRowCount(ctx, count)
	if err != nil {
		return err
	}
	key := runtimestate.ProviderKeyPrefix + providerType

	// Check existence if not replacing
	if !replace {
		var existing json.RawMessage
		if err := storeInstance.GetKV(ctx, key, &existing); err == nil {
			return fmt.Errorf("provider config already exists")
		} else if !errors.Is(err, libdb.ErrNotFound) {
			return fmt.Errorf("failed to check existing config: %w", err)
		}
	}

	// Prepare and store config
	config.Type = providerType
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := storeInstance.SetKV(ctx, key, data); err != nil {
		return fmt.Errorf("failed to store config: %w", err)
	}

	// Handle backend configuration
	backendURL := ""
	switch providerType {
	case ProviderTypeOpenAI:
		backendURL = "https://api.openai.com/v1"
	case ProviderTypeGemini:
		backendURL = "https://generativelanguage.googleapis.com"
	}

	// Upsert backend configuration
	backend := &runtimetypes.Backend{
		ID:      providerType,
		Name:    providerType,
		BaseURL: backendURL,
		Type:    providerType,
	}

	if _, err := storeInstance.GetBackend(ctx, providerType); errors.Is(err, libdb.ErrNotFound) {
		if err := storeInstance.CreateBackend(ctx, backend); err != nil {
			return fmt.Errorf("failed to create backend: %w", err)
		}
	} else if err == nil && replace {
		// Update existing backend if replacing
		if err := storeInstance.UpdateBackend(ctx, backend); err != nil {
			return fmt.Errorf("failed to update backend: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to check backend existence: %w", err)
	}

	return com(ctx)
}

func (s *service) GetProviderConfig(ctx context.Context, providerType string) (*runtimestate.ProviderConfig, error) {
	tx := s.dbInstance.WithoutTransaction()
	var config runtimestate.ProviderConfig
	key := runtimestate.ProviderKeyPrefix + providerType
	storeInstance := runtimetypes.New(tx)
	err := storeInstance.GetKV(ctx, key, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func (s *service) DeleteProviderConfig(ctx context.Context, providerType string) error {
	tx := s.dbInstance.WithoutTransaction()
	storeInstance := runtimetypes.New(tx)

	key := runtimestate.ProviderKeyPrefix + providerType
	return storeInstance.DeleteKV(ctx, key)
}

func (s *service) ListProviderConfigs(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimestate.ProviderConfig, error) {
	tx := s.dbInstance.WithoutTransaction()
	storeInstance := runtimetypes.New(tx)

	kvs, err := storeInstance.ListKVPrefix(ctx, runtimestate.ProviderKeyPrefix, createdAtCursor, limit)
	if err != nil {
		return nil, err
	}

	var configs []*runtimestate.ProviderConfig
	for _, kv := range kvs {
		var config runtimestate.ProviderConfig
		if err := json.Unmarshal(kv.Value, &config); err == nil {
			configs = append(configs, &config)
		}
	}
	return configs, nil
}
