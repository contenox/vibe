package messagestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/vibe/libdbexec"
)

var ErrNotFound = errors.New("not found")

type store struct {
	Exec libdbexec.Exec
}

// New creates a new message store instance.
func New(exec libdbexec.Exec) Store {
	return &store{Exec: exec}
}

// CreateMessageIndex creates a new message index (unnamed).
func (s *store) CreateMessageIndex(ctx context.Context, id string, identity string) error {
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO message_indices(id, identity)
		VALUES ($1, $2)`,
		id,
		identity,
	)
	if err != nil {
		return fmt.Errorf("failed to create message index: %w", err)
	}
	return nil
}

// CreateNamedMessageIndex creates a new message index with a human-readable name.
func (s *store) CreateNamedMessageIndex(ctx context.Context, id string, identity string, name string) error {
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO message_indices(id, identity, name)
		VALUES ($1, $2, $3)`,
		id,
		identity,
		name,
	)
	if err != nil {
		return fmt.Errorf("failed to create named message index: %w", err)
	}
	return nil
}

// DeleteMessageIndex deletes a message index.
func (s *store) DeleteMessageIndex(ctx context.Context, id string, identity string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM message_indices
		WHERE id = $1 AND identity = $2`,
		id,
		identity,
	)
	if err != nil {
		return fmt.Errorf("failed to delete message index: %w", err)
	}
	return checkRowsAffected(result)
}

// ListMessageStreams lists all message stream IDs for an identity.
func (s *store) ListMessageStreams(ctx context.Context, identity string) ([]string, error) {
	return s.listMessageIndicesByIdentity(ctx, identity)
}

// ListMessageIndices lists all message index IDs for an identity.
func (s *store) ListMessageIndices(ctx context.Context, identity string) ([]string, error) {
	return s.listMessageIndicesByIdentity(ctx, identity)
}

func (s *store) listMessageIndicesByIdentity(ctx context.Context, identity string) ([]string, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id
		FROM message_indices
		WHERE identity = $1`,
		identity,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query message indices: %w", err)
	}
	defer rows.Close()

	var streams []string
	for rows.Next() {
		var stream string
		if err := rows.Scan(&stream); err != nil {
			return nil, fmt.Errorf("failed to scan message indices: %w", err)
		}
		streams = append(streams, stream)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return streams, nil
}

// ListAllSessions lists all session info rows for an identity.
func (s *store) ListAllSessions(ctx context.Context, identity string) ([]SessionInfo, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id, identity, COALESCE(name, '')
		FROM message_indices
		WHERE identity = $1
		ORDER BY id ASC`,
		identity,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query sessions: %w", err)
	}
	defer rows.Close()

	var sessions []SessionInfo
	for rows.Next() {
		var si SessionInfo
		if err := rows.Scan(&si.ID, &si.Identity, &si.Name); err != nil {
			return nil, fmt.Errorf("failed to scan session: %w", err)
		}
		sessions = append(sessions, si)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return sessions, nil
}

// GetSessionByName returns the session with the given name for an identity.
func (s *store) GetSessionByName(ctx context.Context, identity string, name string) (*SessionInfo, error) {
	var si SessionInfo
	err := s.Exec.QueryRowContext(ctx, `
		SELECT id, identity, COALESCE(name, '')
		FROM message_indices
		WHERE identity = $1 AND name = $2`,
		identity,
		name,
	).Scan(&si.ID, &si.Identity, &si.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session by name: %w", err)
	}
	return &si, nil
}

// RenameSession updates the human-readable name of a session.
func (s *store) RenameSession(ctx context.Context, id string, name string) error {
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE message_indices
		SET name = $2
		WHERE id = $1`,
		id,
		name,
	)
	if err != nil {
		return fmt.Errorf("failed to rename session: %w", err)
	}
	return checkRowsAffected(result)
}

// AppendMessages appends multiple messages in a single batch insert.
func (s *store) AppendMessages(ctx context.Context, messages ...*Message) error {
	if len(messages) == 0 {
		return nil
	}

	now := time.Now().UTC()
	valueStrings := make([]string, 0, len(messages))
	valueArgs := make([]any, 0, len(messages)*4)

	for i, msg := range messages {
		if msg.AddedAt.IsZero() {
			msg.AddedAt = now
		}
		valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d)", i*4+1, i*4+2, i*4+3, i*4+4))
		valueArgs = append(valueArgs, msg.ID, msg.IDX, msg.Payload, msg.AddedAt)
	}

	stmt := fmt.Sprintf(`
		INSERT INTO messages (id, idx_id, payload, added_at)
		VALUES %s`,
		strings.Join(valueStrings, ","),
	)

	_, err := s.Exec.ExecContext(ctx, stmt, valueArgs...)
	if err != nil {
		return fmt.Errorf("failed to append messages: %w", err)
	}
	return nil
}

// DeleteMessages deletes all messages for a stream.
func (s *store) DeleteMessages(ctx context.Context, stream string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM messages
		WHERE idx_id = $1`,
		stream,
	)
	if err != nil {
		return fmt.Errorf("failed to delete messages: %w", err)
	}
	return checkRowsAffected(result)
}

// ListMessages lists all messages for a stream in chronological order.
func (s *store) ListMessages(ctx context.Context, stream string) ([]*Message, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id, idx_id, payload, added_at
		FROM messages
		WHERE idx_id = $1
		ORDER BY added_at ASC`,
		stream,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query messages: %w", err)
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.IDX, &msg.Payload, &msg.AddedAt); err != nil {
			return nil, fmt.Errorf("failed to scan messages: %w", err)
		}
		msgs = append(msgs, &msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return msgs, nil
}

// LastMessage gets the most recent message for a stream.
func (s *store) LastMessage(ctx context.Context, stream string) (*Message, error) {
	row := s.Exec.QueryRowContext(ctx, `
		SELECT id, idx_id, payload, added_at
		FROM messages
		WHERE idx_id = $1
		ORDER BY added_at DESC
		LIMIT 1`,
		stream,
	)

	var msg Message
	if err := row.Scan(&msg.ID, &msg.IDX, &msg.Payload, &msg.AddedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get last message: %w", err)
	}
	return &msg, nil
}

// CountMessages returns the number of messages for a stream.
func (s *store) CountMessages(ctx context.Context, stream string) (int, error) {
	var count int
	err := s.Exec.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM messages
		WHERE idx_id = $1`,
		stream,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count messages: %w", err)
	}
	return count, nil
}

func checkRowsAffected(result sql.Result) error {
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
