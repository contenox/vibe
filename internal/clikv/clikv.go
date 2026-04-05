// Package clikv reads and writes contenox CLI settings in SQLite KV (prefix "cli.").
// Keys match contenox config set (e.g. default-model, default-provider).
package clikv

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/contenox/contenox/runtimetypes"
)

// Prefix is the KV key prefix for CLI-level settings.
const Prefix = "cli."

// Read returns the trimmed string value for key (e.g. "default-model"), or "" if missing.
func Read(ctx context.Context, store runtimetypes.Store, key string) string {
	var val string
	if err := store.GetKV(ctx, Prefix+key, &val); err != nil {
		return ""
	}
	return strings.TrimSpace(val)
}

// SetString persists a string value for key, JSON-encoded like contenox config set.
func SetString(ctx context.Context, store runtimetypes.Store, key, value string) error {
	v := strings.TrimSpace(value)
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return store.SetKV(ctx, Prefix+key, json.RawMessage(data))
}
