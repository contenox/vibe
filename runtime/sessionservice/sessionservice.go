// Package sessionservice provides CRUD operations for CLI chat sessions.
// It encapsulates messagestore + runtimetypes KV orchestration so session_cmd
// and chat flows can share logic without duplicating raw transaction management.
package sessionservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/messagestore"
	"github.com/contenox/contenox/runtime/runtimetypes"
)

const kvActiveSession = "contenox.session.active"

// SessionInfo summarises a session for listing purposes.
type SessionInfo struct {
	ID           string
	Name         string
	MessageCount int
	IsActive     bool
}

// Service is the session management interface.
type Service interface {
	// New creates a new named session and makes it active.
	// If name is empty, a UUID-based name is generated.
	New(ctx context.Context, identity, name string) (id string, err error)

	// List returns all sessions for the given identity with message counts.
	// The currently active session is marked IsActive = true.
	List(ctx context.Context, identity string) ([]*SessionInfo, error)

	// Switch changes the active session pointer to the session with the given name.
	Switch(ctx context.Context, identity, name string) error

	// Delete removes a session and its messages. Reports whether the session
	// that was deleted was the currently active one.
	Delete(ctx context.Context, identity, name string) (wasActive bool, err error)

	// GetActiveID reads the active session ID from the KV store.
	// Returns ("", nil) when no session is active.
	GetActiveID(ctx context.Context) (string, error)

	// SetActiveID persists an active session ID to the KV store.
	SetActiveID(ctx context.Context, id string) error

	// EnsureDefault creates (or reuses) a "default" session, sets it active,
	// and returns its ID. Idempotent.
	EnsureDefault(ctx context.Context, identity string) (string, error)
}

type service struct {
	db libdb.DBManager
}

// New returns a Service backed by the given database manager.
func New(db libdb.DBManager) Service {
	return &service{db: db}
}


func (s *service) New(ctx context.Context, identity, name string) (string, error) {
	if name == "" {
		name = "session-" + uuid.New().String()[:8]
	}
	exec := s.db.WithoutTransaction()
	if _, err := messagestore.New(exec).GetSessionByName(ctx, identity, name); err == nil {
		return "", fmt.Errorf("session %q already exists", name)
	}

	newID := uuid.New().String()
	txExec, commit, release, txErr := s.db.WithTransaction(ctx)
	if txErr != nil {
		return "", fmt.Errorf("failed to start transaction: %w", txErr)
	}
	defer release()

	if err := messagestore.New(txExec).CreateNamedMessageIndex(ctx, newID, identity, name); err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}
	if err := s.setKV(ctx, txExec, newID); err != nil {
		return "", fmt.Errorf("failed to set active session: %w", err)
	}
	if err := commit(ctx); err != nil {
		return "", fmt.Errorf("failed to commit: %w", err)
	}
	return newID, nil
}

func (s *service) List(ctx context.Context, identity string) ([]*SessionInfo, error) {
	exec := s.db.WithoutTransaction()
	sessions, err := messagestore.New(exec).ListAllSessions(ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	activeID, _ := s.GetActiveID(ctx)
	store := messagestore.New(exec)
	out := make([]*SessionInfo, 0, len(sessions))
	for _, sess := range sessions {
		count, _ := store.CountMessages(ctx, sess.ID)
		out = append(out, &SessionInfo{
			ID:           sess.ID,
			Name:         sess.Name,
			MessageCount: count,
			IsActive:     sess.ID == activeID,
		})
	}
	return out, nil
}

func (s *service) Switch(ctx context.Context, identity, name string) error {
	exec := s.db.WithoutTransaction()
	si, err := messagestore.New(exec).GetSessionByName(ctx, identity, name)
	if err != nil {
		if errors.Is(err, messagestore.ErrNotFound) {
			return fmt.Errorf("session %q not found", name)
		}
		return fmt.Errorf("failed to look up session: %w", err)
	}
	return s.setKV(ctx, exec, si.ID)
}

func (s *service) Delete(ctx context.Context, identity, name string) (bool, error) {
	exec := s.db.WithoutTransaction()
	si, err := messagestore.New(exec).GetSessionByName(ctx, identity, name)
	if err != nil {
		if errors.Is(err, messagestore.ErrNotFound) {
			return false, fmt.Errorf("session %q not found", name)
		}
		return false, fmt.Errorf("failed to look up session: %w", err)
	}

	activeID, _ := s.GetActiveID(ctx)
	wasActive := activeID == si.ID

	txExec, commit, release, txErr := s.db.WithTransaction(ctx)
	if txErr != nil {
		return false, fmt.Errorf("failed to start transaction: %w", txErr)
	}
	defer release()

	if err := messagestore.New(txExec).DeleteMessageIndex(ctx, si.ID, identity); err != nil {
		return false, fmt.Errorf("failed to delete session: %w", err)
	}
	if wasActive {
		_ = s.setKV(ctx, txExec, "") // clear active pointer; best-effort
	}
	if err := commit(ctx); err != nil {
		return false, fmt.Errorf("failed to commit: %w", err)
	}
	return wasActive, nil
}

func (s *service) GetActiveID(ctx context.Context) (string, error) {
	var id string
	if err := runtimetypes.New(s.db.WithoutTransaction()).GetKV(ctx, kvActiveSession, &id); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read active session: %w", err)
	}
	return id, nil
}

func (s *service) SetActiveID(ctx context.Context, id string) error {
	return s.setKV(ctx, s.db.WithoutTransaction(), id)
}

func (s *service) EnsureDefault(ctx context.Context, identity string) (string, error) {
	const defaultName = "default"
	exec := s.db.WithoutTransaction()

	activeID, err := s.GetActiveID(ctx)
	if err != nil {
		return "", err
	}
	if activeID != "" {
		sessions, err := messagestore.New(exec).ListAllSessions(ctx, identity)
		if err == nil {
			for _, sess := range sessions {
				if sess.ID == activeID {
					return activeID, nil // active session exists and is valid
				}
			}
		}
		slog.Warn("Active session not found in DB, re-creating default", "activeID", activeID)
	}

	// Re-use an existing "default" session if present.
	if existing, err := messagestore.New(exec).GetSessionByName(ctx, identity, defaultName); err == nil {
		if setErr := s.setKV(ctx, exec, existing.ID); setErr != nil {
			slog.Warn("Failed to set active session", "error", setErr)
		}
		return existing.ID, nil
	}

	// Create a fresh default session.
	newID := uuid.New().String()
	txExec, commit, release, txErr := s.db.WithTransaction(ctx)
	if txErr != nil {
		return "", fmt.Errorf("failed to start transaction: %w", txErr)
	}
	defer release()

	if err := messagestore.New(txExec).CreateNamedMessageIndex(ctx, newID, identity, defaultName); err != nil {
		return "", fmt.Errorf("failed to create default session: %w", err)
	}
	if err := s.setKV(ctx, txExec, newID); err != nil {
		return "", fmt.Errorf("failed to activate default session: %w", err)
	}
	if err := commit(ctx); err != nil {
		return "", fmt.Errorf("failed to commit default session: %w", err)
	}
	return newID, nil
}

// setKV writes id as a JSON string to the KV table using the given executor.
func (s *service) setKV(ctx context.Context, exec libdb.Exec, id string) error {
	raw, err := json.Marshal(id)
	if err != nil {
		return fmt.Errorf("failed to marshal session id: %w", err)
	}
	return runtimetypes.New(exec).SetKV(ctx, kvActiveSession, raw)
}
