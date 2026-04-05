package taskchainservice

import (
	"context"

	"github.com/contenox/contenox/taskengine"
)

// Service loads and stores task chain definitions as JSON in the VFS (see NewVFS).
// Get accepts either a relative VFS path (e.g. default-chain.json) or a logical chain id
// (inner JSON "id") by scanning root-level *.json files.
type Service interface {
	Get(ctx context.Context, ref string) (*taskengine.TaskChainDefinition, error)
	CreateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error
	UpdateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error
	DeleteByPath(ctx context.Context, path string) error
}
