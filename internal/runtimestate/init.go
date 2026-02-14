package runtimestate

import (
	"context"
	"errors"
	"fmt"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/google/uuid"
)

// Config holds the configuration for the runtime state initializer.
type Config struct {
	DatabaseURL string `json:"database_url"`
	EmbedModel  string `json:"embed_model"`
	TaskModel   string `json:"task_model"`
	ChatModel   string `json:"chat_model"`
	TenantID    string `json:"tenant_id"`
}

const (
	EmbedgroupID   = "internal_embed_group"
	EmbedgroupName = "Embedder"
)

const (
	TasksgroupID   = "internal_tasks_group"
	TasksgroupName = "Tasks"
)

const (
	ChatgroupID   = "internal_chat_group"
	ChatgroupName = "Chat"
)

type modelCapability int

const (
	canEmbed modelCapability = iota
	canPrompt
	canChat
)

// InitEmbeder initializes the embedding group and its designated model.
func InitEmbeder(ctx context.Context, config *Config, dbInstance libdb.DBManager, contextLen int, runtime *State) error {
	tx, com, r, err := dbInstance.WithTransaction(ctx)
	if err != nil {
		return err
	}
	defer r()

	if contextLen <= 0 {
		return fmt.Errorf("invalid context length")
	}
	group, err := initEmbedgroup(ctx, config, tx, false)
	if err != nil {
		return fmt.Errorf("init embed group: %w", err)
	}
	model, err := initEmbedModel(ctx, config, tx, contextLen)
	if err != nil {
		return fmt.Errorf("init embed model: %w", err)
	}
	if err = assignModelTogroup(ctx, config, tx, model, group); err != nil {
		return fmt.Errorf("assign embed model to group: %w", err)
	}
	return com(ctx)
}

// InitPromptExec initializes the tasks group and its designated model.
func InitPromptExec(ctx context.Context, config *Config, dbInstance libdb.DBManager, runtime *State, contextLen int) error {
	tx, com, r, err := dbInstance.WithTransaction(ctx)
	if err != nil {
		return err
	}
	defer r()

	if contextLen <= 0 {
		return fmt.Errorf("invalid context length")
	}
	group, err := initTaskgroup(ctx, config, tx, false)
	if err != nil {
		return fmt.Errorf("init task group: %w", err)
	}
	model, err := initTaskModel(ctx, config, tx, contextLen)
	if err != nil {
		return fmt.Errorf("init task model: %w", err)
	}
	if err = assignModelTogroup(ctx, config, tx, model, group); err != nil {
		return fmt.Errorf("assign task model to group: %w", err)
	}
	return com(ctx)
}

// InitChatExec initializes the chat group and its designated model.
func InitChatExec(ctx context.Context, config *Config, dbInstance libdb.DBManager, runtime *State, contextLen int) error {
	tx, com, r, err := dbInstance.WithTransaction(ctx)
	if err != nil {
		return err
	}
	defer r()

	if contextLen <= 0 {
		return fmt.Errorf("invalid context length")
	}
	group, err := initChatgroup(ctx, config, tx, false)
	if err != nil {
		return fmt.Errorf("init chat group: %w", err)
	}
	model, err := initChatModel(ctx, config, tx, contextLen)
	if err != nil {
		return fmt.Errorf("init chat model: %w", err)
	}
	if err = assignModelTogroup(ctx, config, tx, model, group); err != nil {
		return fmt.Errorf("assign chat model to group: %w", err)
	}
	return com(ctx)
}

func initEmbedgroup(ctx context.Context, config *Config, tx libdb.Exec, created bool) (*runtimetypes.AffinityGroup, error) {
	group, err := runtimetypes.New(tx).GetAffinityGroup(ctx, EmbedgroupID)
	if !created && errors.Is(err, libdb.ErrNotFound) {
		err = runtimetypes.New(tx).CreateAffinityGroup(ctx, &runtimetypes.AffinityGroup{
			ID:          EmbedgroupID,
			Name:        EmbedgroupName,
			PurposeType: "Internal Embeddings",
		})
		if err != nil {
			return nil, err
		}
		return initEmbedgroup(ctx, config, tx, true)
	}
	if err != nil {
		return nil, err
	}
	return group, nil
}

func initTaskgroup(ctx context.Context, config *Config, tx libdb.Exec, created bool) (*runtimetypes.AffinityGroup, error) {
	group, err := runtimetypes.New(tx).GetAffinityGroup(ctx, TasksgroupID)
	if !created && errors.Is(err, libdb.ErrNotFound) {
		err = runtimetypes.New(tx).CreateAffinityGroup(ctx, &runtimetypes.AffinityGroup{
			ID:          TasksgroupID,
			Name:        TasksgroupName,
			PurposeType: "Internal Tasks",
		})
		if err != nil {
			return nil, err
		}
		return initTaskgroup(ctx, config, tx, true)
	}
	if err != nil {
		return nil, err
	}
	return group, nil
}

func initChatgroup(ctx context.Context, config *Config, tx libdb.Exec, created bool) (*runtimetypes.AffinityGroup, error) {
	group, err := runtimetypes.New(tx).GetAffinityGroup(ctx, ChatgroupID)
	if !created && errors.Is(err, libdb.ErrNotFound) {
		err = runtimetypes.New(tx).CreateAffinityGroup(ctx, &runtimetypes.AffinityGroup{
			ID:          ChatgroupID,
			Name:        ChatgroupName,
			PurposeType: "Internal Chat",
		})
		if err != nil {
			return nil, err
		}
		return initChatgroup(ctx, config, tx, true)
	}
	if err != nil {
		return nil, err
	}
	return group, nil
}

// initOrUpdateModel is a generic helper that handles the creation or update of a model.
// It ensures a model is created if it doesn't exist or updated with a new capability if it does.
// It returns an error if an existing model has a conflicting context length.
func initOrUpdateModel(ctx context.Context, tx libdb.Exec, tenantID, modelName string, contextLength int, capability modelCapability) (*runtimetypes.Model, error) {
	if modelName == "" {
		return nil, errors.New("model name cannot be empty")
	}
	parsedTenantID, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("invalid tenant_id: %w", err)
	}
	modelID := uuid.NewSHA1(parsedTenantID, []byte(modelName))
	storeInstance := runtimetypes.New(tx)

	// Attempt to retrieve the model by its unique name
	model, err := storeInstance.GetModelByName(ctx, modelName)

	// Case 1: Model does not exist, so we create it.
	if errors.Is(err, libdb.ErrNotFound) {
		newModel := &runtimetypes.Model{
			Model:         modelName,
			ID:            modelID.String(),
			ContextLength: contextLength,
		}
		switch capability {
		case canEmbed:
			newModel.CanEmbed = true
		case canPrompt:
			newModel.CanPrompt = true
		case canChat:
			newModel.CanChat = true
		}
		if err := storeInstance.AppendModel(ctx, newModel); err != nil {
			return nil, fmt.Errorf("failed to append new model '%s': %w", modelName, err)
		}
		return newModel, nil
	}

	// Case 2: An unexpected database error occurred.
	if err != nil {
		return nil, fmt.Errorf("failed to get model '%s': %w", modelName, err)
	}

	// Case 3: Model exists. Validate and update its capabilities if needed.
	if model.ContextLength != contextLength {
		return nil, fmt.Errorf("model '%s' already exists with a different context length (stored: %d, new: %d)", modelName, model.ContextLength, contextLength)
	}

	needsUpdate := false
	switch capability {
	case canEmbed:
		if !model.CanEmbed {
			model.CanEmbed = true
			needsUpdate = true
		}
	case canPrompt:
		if !model.CanPrompt {
			model.CanPrompt = true
			needsUpdate = true
		}
	case canChat:
		if !model.CanChat {
			model.CanChat = true
			needsUpdate = true
		}
	}

	if needsUpdate {
		if err := storeInstance.UpdateModel(ctx, model); err != nil {
			return nil, fmt.Errorf("failed to update model '%s' capabilities: %w", modelName, err)
		}
	}

	return model, nil
}

func initEmbedModel(ctx context.Context, config *Config, tx libdb.Exec, contextLength int) (*runtimetypes.Model, error) {
	return initOrUpdateModel(ctx, tx, config.TenantID, config.EmbedModel, contextLength, canEmbed)
}

func initTaskModel(ctx context.Context, config *Config, tx libdb.Exec, contextLength int) (*runtimetypes.Model, error) {
	return initOrUpdateModel(ctx, tx, config.TenantID, config.TaskModel, contextLength, canPrompt)
}

func initChatModel(ctx context.Context, config *Config, tx libdb.Exec, contextLength int) (*runtimetypes.Model, error) {
	return initOrUpdateModel(ctx, tx, config.TenantID, config.ChatModel, contextLength, canChat)
}

// ExtraModelSpec describes an extra model to ensure exists in ollama_models (e.g. for contenox-vibe extra_models config).
type ExtraModelSpec struct {
	Name          string
	ContextLength int
	CanChat       bool
	CanPrompt     bool
	CanEmbed      bool
}

// EnsureModels ensures each given model exists in ollama_models with the specified context length and capabilities.
// It is intended for contenox-vibe so that extra models (e.g. qwen2.5:7b for the vibes chain) are declared and
// get correct context/capabilities during backend sync. Call after InitEmbeder/InitPromptExec/InitChatExec and before RunBackendCycle.
func EnsureModels(ctx context.Context, dbInstance libdb.DBManager, tenantID string, specs []ExtraModelSpec) error {
	if len(specs) == 0 {
		return nil
	}
	tx, com, r, err := dbInstance.WithTransaction(ctx)
	if err != nil {
		return err
	}
	defer r()
	for _, spec := range specs {
		if spec.Name == "" {
			return fmt.Errorf("extra_models entry has empty name")
		}
		if spec.ContextLength <= 0 {
			return fmt.Errorf("extra_models entry %q has invalid context length %d", spec.Name, spec.ContextLength)
		}
		if spec.CanEmbed {
			if _, err := initOrUpdateModel(ctx, tx, tenantID, spec.Name, spec.ContextLength, canEmbed); err != nil {
				return fmt.Errorf("extra model %s: %w", spec.Name, err)
			}
		}
		if spec.CanPrompt {
			if _, err := initOrUpdateModel(ctx, tx, tenantID, spec.Name, spec.ContextLength, canPrompt); err != nil {
				return fmt.Errorf("extra model %s: %w", spec.Name, err)
			}
		}
		if spec.CanChat {
			if _, err := initOrUpdateModel(ctx, tx, tenantID, spec.Name, spec.ContextLength, canChat); err != nil {
				return fmt.Errorf("extra model %s: %w", spec.Name, err)
			}
		}
	}
	return com(ctx)
}

func assignModelTogroup(ctx context.Context, _ *Config, tx libdb.Exec, model *runtimetypes.Model, group *runtimetypes.AffinityGroup) error {
	storeInstance := runtimetypes.New(tx)
	models, err := storeInstance.ListModelsForAffinityGroup(ctx, group.ID)
	if err != nil {
		return err
	}
	for _, presentModel := range models {
		if presentModel.ID == model.ID {
			return nil
		}
	}
	if err := storeInstance.AssignModelToAffinityGroup(ctx, group.ID, model.ID); err != nil {
		return err
	}
	return nil
}
