// session.go — active session pointer helpers (reads/writes the SQLite kv table).
package vibecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/messagestore"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/google/uuid"
)

// marshalJSON is a small helper to produce a json.RawMessage.
func marshalJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal value: %w", err)
	}
	return b, nil
}

const (
	kvActiveSession    = "vibe.session.active"
	localIdentity      = "local-user"
	defaultSessionName = "default"
)

// getActiveSessionID reads the active session ID from the kv table.
// Returns ("", nil) if no active session has been set yet.
func getActiveSessionID(ctx context.Context, exec libdb.Exec) (string, error) {
	store := runtimetypes.New(exec)
	var id string
	if err := store.GetKV(ctx, kvActiveSession, &id); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read active session: %w", err)
	}
	return id, nil
}

// setActiveSessionID persists the active session ID to the kv table.
func setActiveSessionID(ctx context.Context, exec libdb.Exec, id string) error {
	store := runtimetypes.New(exec)
	raw, err := marshalJSON(id)
	if err != nil {
		return err
	}
	return store.SetKV(ctx, kvActiveSession, raw)
}

// ensureDefaultSession creates the "default" session if no active session exists,
// sets it as active, and returns the session ID to use for this invocation.
func ensureDefaultSession(ctx context.Context, db libdb.DBManager) (string, error) {
	exec := db.WithoutTransaction()
	// Check if there's already an active session.
	activeID, err := getActiveSessionID(ctx, exec)
	if err != nil {
		return "", err
	}
	if activeID != "" {
		// Verify it still exists.
		sessions, err := messagestore.New(exec).ListAllSessions(ctx, localIdentity)
		if err == nil {
			for _, s := range sessions {
				if s.ID == activeID {
					return activeID, nil
				}
			}
		}
		slog.Warn("Active session not found in DB, re-creating default", "activeID", activeID)
	}

	// Look for an existing "default" session.
	existing, err := messagestore.New(exec).GetSessionByName(ctx, localIdentity, defaultSessionName)
	if err == nil {
		// Found — make it active.
		if setErr := setActiveSessionID(ctx, exec, existing.ID); setErr != nil {
			slog.Warn("Failed to set active session", "error", setErr)
		}
		return existing.ID, nil
	}

	// None found — create a new default session.
	newID := uuid.New().String()
	txExec, commit, release, txErr := db.WithTransaction(ctx)
	if txErr != nil {
		return "", fmt.Errorf("failed to start transaction: %w", txErr)
	}
	defer release()

	if err := messagestore.New(txExec).CreateNamedMessageIndex(ctx, newID, localIdentity, defaultSessionName); err != nil {
		return "", fmt.Errorf("failed to create default session: %w", err)
	}
	if err := setActiveSessionID(ctx, txExec, newID); err != nil {
		return "", fmt.Errorf("failed to set active session: %w", err)
	}
	if err := commit(ctx); err != nil {
		return "", fmt.Errorf("failed to commit default session: %w", err)
	}
	return newID, nil
}
