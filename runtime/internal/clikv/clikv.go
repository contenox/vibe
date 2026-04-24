package clikv

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/contenox/contenox/runtime/runtimetypes"
)

const Prefix = "cli."

var workspaceScopedKeys = map[string]bool{
	"default-chain":    true,
	"hitl-policy-name": true,
}

func Read(ctx context.Context, store runtimetypes.Store, key string) string {
	var val string
	if err := store.GetKV(ctx, Prefix+key, &val); err != nil {
		return ""
	}
	return strings.TrimSpace(val)
}

// ReadConfig reads key using workspace scope with global fallback for workspace-scoped keys.
// Returns (value, "workspace"|"global").
func ReadConfig(ctx context.Context, store runtimetypes.Store, workspaceID, key string) (string, string) {
	if workspaceScopedKeys[key] && workspaceID != "" {
		var val string
		if err := store.GetWorkspaceKV(ctx, workspaceID, Prefix+key, &val); err == nil {
			if v := strings.TrimSpace(val); v != "" {
				return v, "workspace"
			}
		}
	}
	return Read(ctx, store, key), "global"
}

func ReadHITLPolicy(ctx context.Context, store runtimetypes.Store) string {
	return Read(ctx, store, "hitl-policy-name")
}

func SetHITLPolicy(ctx context.Context, store runtimetypes.Store, name string) error {
	return SetString(ctx, store, "hitl-policy-name", name)
}

func SetString(ctx context.Context, store runtimetypes.Store, key, value string) error {
	v := strings.TrimSpace(value)
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return store.SetKV(ctx, Prefix+key, json.RawMessage(data))
}

// WriteConfig writes key to workspace scope for workspace-scoped keys, global scope otherwise.
func WriteConfig(ctx context.Context, store runtimetypes.Store, workspaceID, key, value string) error {
	v := strings.TrimSpace(value)
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if workspaceScopedKeys[key] && workspaceID != "" {
		return store.SetWorkspaceKV(ctx, workspaceID, Prefix+key, json.RawMessage(data))
	}
	return store.SetKV(ctx, Prefix+key, json.RawMessage(data))
}
