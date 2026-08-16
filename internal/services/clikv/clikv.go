// Package clikv reads and writes the CLI's persisted settings (the cli.* KV
// namespace) and owns the scope of every one of them. scopeFor is the single
// place a key's row location is decided, so a reader and a writer of the same
// key cannot target different rows.
package clikv

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Prefix namespaces every CLI setting inside the KV table.
const Prefix = "cli."

// Workspace-scoped settings: each names a file inside the project's
// .contenox/, so its value means something different per project.
const (
	// KeyDefaultChain is the chain file a run falls back to.
	KeyDefaultChain = "default-chain"
	// KeyHITLPolicyName is the HITL policy the evaluator gates tool calls with.
	KeyHITLPolicyName = "hitl-policy-name"
)

var workspaceScopedKeys = map[string]bool{
	KeyDefaultChain:   true,
	KeyHITLPolicyName: true,
}

// IsWorkspaceScoped reports whether key's row lives under a workspace rather
// than under the global row.
func IsWorkspaceScoped(key string) bool { return workspaceScopedKeys[key] }

// WorkspaceScopedKeys returns the registered workspace-scoped keys, sorted.
func WorkspaceScopedKeys() []string {
	keys := make([]string, 0, len(workspaceScopedKeys))
	for key := range workspaceScopedKeys {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func scopeFor(key, workspaceID string) string {
	if workspaceScopedKeys[key] {
		return strings.TrimSpace(workspaceID)
	}
	return ""
}

// KVReader reads the global row. runtimetypes.Store satisfies it, as does any
// narrower store that can reach only that row.
type KVReader interface {
	GetKV(ctx context.Context, key string, out any) error
}

// Reader adds the workspace row, which ReadConfig needs to resolve a
// workspace-scoped key at its own scope. A KVReader-only store sees the
// global row and nothing else.
type Reader interface {
	KVReader
	GetWorkspaceKV(ctx context.Context, workspaceID string, key string, out any) error
}

// Writer is the write surface. runtimetypes.Store satisfies it.
type Writer interface {
	SetKV(ctx context.Context, key string, value json.RawMessage) error
	SetWorkspaceKV(ctx context.Context, workspaceID string, key string, value json.RawMessage) error
}

// Read returns key's global-row value, or "" when unset or unreadable. For a
// workspace-scoped key this is only ReadConfig's fallback leg, never the
// whole answer — read those through ReadConfig.
func Read(ctx context.Context, store KVReader, key string) string {
	var val string
	if err := store.GetKV(ctx, Prefix+key, &val); err != nil {
		return ""
	}
	return strings.TrimSpace(val)
}

// ReadConfig returns key's value for a caller in workspaceID and the scope it
// came from ("workspace" or "global"), falling back to the global row when the
// workspace row is unset.
func ReadConfig(ctx context.Context, store Reader, workspaceID, key string) (string, string) {
	if ws := scopeFor(key, workspaceID); ws != "" {
		var val string
		if err := store.GetWorkspaceKV(ctx, ws, Prefix+key, &val); err == nil {
			if v := strings.TrimSpace(val); v != "" {
				return v, "workspace"
			}
		}
	}
	return Read(ctx, store, key), "global"
}

// ReadHITLPolicy returns the HITL policy file name active for workspaceID —
// the value the evaluator gates on. Empty means the caller's fallback policy.
func ReadHITLPolicy(ctx context.Context, store Reader, workspaceID string) string {
	val, _ := ReadConfig(ctx, store, workspaceID, KeyHITLPolicyName)
	return val
}

// SetHITLPolicy writes the active HITL policy file name for workspaceID
// through WriteConfig, so ACP's /policy lands in the row `contenox config set
// hitl-policy-name` writes and the evaluator reads.
func SetHITLPolicy(ctx context.Context, store Writer, workspaceID, name string) error {
	return WriteConfig(ctx, store, workspaceID, KeyHITLPolicyName, name)
}

// SetString writes key's global row. It rejects a workspace-scoped key rather
// than writing a row no workspace-aware reader would look at: those have one
// door, WriteConfig, which names the workspace ("" for global) explicitly.
func SetString(ctx context.Context, store Writer, key, value string) error {
	if workspaceScopedKeys[key] {
		return fmt.Errorf("clikv: %q is workspace-scoped; write it through WriteConfig with an explicit workspace", key)
	}
	data, err := encode(value)
	if err != nil {
		return err
	}
	return store.SetKV(ctx, Prefix+key, data)
}

// WriteConfig writes key's value into the row scopeFor names for a caller in
// workspaceID — the row ReadConfig reads back.
func WriteConfig(ctx context.Context, store Writer, workspaceID, key, value string) error {
	data, err := encode(value)
	if err != nil {
		return err
	}
	// The empty workspace id IS the global row, so one call covers both
	// scopes and no second write path can drift from scopeFor.
	return store.SetWorkspaceKV(ctx, scopeFor(key, workspaceID), Prefix+key, data)
}

func encode(value string) (json.RawMessage, error) {
	data, err := json.Marshal(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}
