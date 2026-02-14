package taskchainservice

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/taskengine"
)

const (
	taskChainPrefix = "taskchain:"
)

type Service interface {
	// Create a new task chain
	Create(ctx context.Context, chain *taskengine.TaskChainDefinition) error

	// Get a task chain by ID
	Get(ctx context.Context, id string) (*taskengine.TaskChainDefinition, error)

	// Update an existing task chain
	Update(ctx context.Context, chain *taskengine.TaskChainDefinition) error

	// Delete a task chain
	Delete(ctx context.Context, id string) error

	// List task chains with pagination
	List(ctx context.Context, cursor *time.Time, limit int) ([]*taskengine.TaskChainDefinition, error)
}

type service struct {
	db libdb.DBManager
}

func New(db libdb.DBManager) Service {
	return &service{db: db}
}

func (s *service) Create(ctx context.Context, chain *taskengine.TaskChainDefinition) error {
	if chain.ID == "" {
		return fmt.Errorf("task chain ID is required")
	}

	// Validate the chain structure
	if len(chain.Tasks) == 0 {
		return fmt.Errorf("task chain must contain at least one task")
	}

	key := taskChainPrefix + chain.ID
	value, err := json.Marshal(chain)
	if err != nil {
		return fmt.Errorf("failed to serialize task chain: %w", err)
	}
	storeInstance := runtimetypes.New(s.db.WithoutTransaction())
	return storeInstance.SetKV(ctx, key, value)
}

func (s *service) Get(ctx context.Context, id string) (*taskengine.TaskChainDefinition, error) {
	if id == "" {
		return nil, fmt.Errorf("task chain ID is required")
	}

	key := taskChainPrefix + id
	var chain taskengine.TaskChainDefinition
	storeInstance := runtimetypes.New(s.db.WithoutTransaction())

	if err := storeInstance.GetKV(ctx, key, &chain); err != nil {
		return nil, fmt.Errorf("failed to get task chain: %w", err)
	}

	return &chain, nil
}

func (s *service) Update(ctx context.Context, chain *taskengine.TaskChainDefinition) error {
	if chain.ID == "" {
		return fmt.Errorf("task chain ID is required")
	}

	key := taskChainPrefix + chain.ID
	value, err := json.Marshal(chain)
	if err != nil {
		return fmt.Errorf("failed to serialize task chain: %w", err)
	}
	storeInstance := runtimetypes.New(s.db.WithoutTransaction())

	return storeInstance.UpdateKV(ctx, key, value)
}

func (s *service) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("task chain ID is required")
	}

	key := taskChainPrefix + id
	storeInstance := runtimetypes.New(s.db.WithoutTransaction())

	return storeInstance.DeleteKV(ctx, key)
}

func (s *service) List(ctx context.Context, cursor *time.Time, limit int) ([]*taskengine.TaskChainDefinition, error) {
	// Use ListKVPrefix to get all task chains
	storeInstance := runtimetypes.New(s.db.WithoutTransaction())
	kvs, err := storeInstance.ListKVPrefix(ctx, taskChainPrefix, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list task chains: %w", err)
	}

	chains := make([]*taskengine.TaskChainDefinition, 0, len(kvs))
	for _, kv := range kvs {
		var chain taskengine.TaskChainDefinition
		if err := json.Unmarshal(kv.Value, &chain); err != nil {
			// Skip invalid entries but log them
			continue
		}
		chains = append(chains, &chain)
	}

	return chains, nil
}
