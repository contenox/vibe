package runtimetypes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

// Message-page bounds: unset defaults to DefaultMessagePageLimit, and requests above MaxMessagePageLimit are clamped.
const (
	DefaultMessagePageLimit = 100
	MaxMessagePageLimit     = 1000
)

// MessageStore is chat-history persistence scoped to one workspace at construction, taking an Exec so callers can compose it into their own transaction.
type MessageStore interface {
	// CreateMessageIndex creates an unnamed conversation index for identity.
	CreateMessageIndex(ctx context.Context, id string, identity string) error
	// CreateNamedMessageIndex creates a named conversation index; the name must be unique per workspace.
	CreateNamedMessageIndex(ctx context.Context, id string, identity string, name string) error
	// DeleteMessageIndex removes identity's index, cascading to its messages, and returns libdb.ErrNotFound when no row matched.
	DeleteMessageIndex(ctx context.Context, id string, identity string) error
	// ListMessageIndices returns identity's index IDs in this workspace.
	ListMessageIndices(ctx context.Context, identity string) ([]string, error)
	// ListMessageSessions returns identity's indices with their message counts
	// and last-activity times, most recently active first; never-used sessions
	// sort last, then by name and ID.
	ListMessageSessions(ctx context.Context, identity string) ([]MessageSession, error)
	// GetMessageSessionByName returns identity's session with that name, or libdb.ErrNotFound when none exists.
	GetMessageSessionByName(ctx context.Context, identity string, name string) (*MessageSession, error)
	// GetMessageIndexName returns an index's name by ID (empty if unset), or libdb.ErrNotFound when no row matched.
	GetMessageIndexName(ctx context.Context, id string) (string, error)
	// RenameMessageSession sets a session's name, returning libdb.ErrNotFound when no row matched.
	RenameMessageSession(ctx context.Context, id string, name string) error

	// AppendMessages inserts messages in one batch statement, stamping a zero AddedAt with now (UTC); empty input is a no-op.
	AppendMessages(ctx context.Context, messages ...*Message) error
	// DeleteMessages removes every message of stream, keeping the index row, and returns libdb.ErrNotFound when the stream held no messages.
	DeleteMessages(ctx context.Context, stream string) error
	// ListMessages returns stream's messages oldest first.
	ListMessages(ctx context.Context, stream string) ([]*Message, error)
	// ListMessagesPage returns one keyset page of stream's messages in (added_at, id) order; a page shorter than the limit is the end of the stream.
	ListMessagesPage(ctx context.Context, stream string, f MessagePageFilter) ([]*Message, error)
	// LastMessage returns stream's newest message, libdb.ErrNotFound when empty.
	LastMessage(ctx context.Context, stream string) (*Message, error)
	// CountMessages returns stream's message count.
	CountMessages(ctx context.Context, stream string) (int, error)
}

// Message is one stored conversation turn; (ID, IDX) is the primary key, so re-appending the same ID to a stream is a constraint error.
type Message struct {
	ID      string    `json:"id"`
	IDX     string    `json:"idx_id"`
	Payload []byte    `json:"payload"`
	AddedAt time.Time `json:"added_at"`
}

// Cursor returns m's page boundary — what a caller passes as
// MessagePageFilter.After to resume the page after (or before) m.
func (m *Message) Cursor() MessageCursor {
	return MessageCursor{AddedAt: m.AddedAt, ID: m.ID}
}

// MessageCursor is one page boundary: the (added_at, id) of the last row a page returned.
type MessageCursor struct {
	AddedAt time.Time
	ID      string
}

// MessagePageFilter narrows a ListMessagesPage read; the zero value is the stream's first page, oldest first, of DefaultMessagePageLimit rows.
type MessagePageFilter struct {
	// After resumes strictly after this boundary (or before it when Backwards); zero starts at the stream's oldest/newest message.
	After MessageCursor
	// Backwards walks the stream newest first.
	Backwards bool
	// Limit caps the page: <= 0 means DefaultMessagePageLimit, more than
	// MaxMessagePageLimit is clamped to it.
	Limit int
}

// MessageSession is one conversation index row plus its aggregates; Name is empty when unnamed and UpdatedAt is zero when the index holds no messages.
type MessageSession struct {
	ID           string
	Identity     string
	Name         string
	MessageCount int
	UpdatedAt    time.Time
}

// MessageIndexRow is one index row with its message count, read across every workspace at once.
type MessageIndexRow struct {
	ID           string
	Identity     string
	WorkspaceID  string
	Name         string
	MessageCount int
}

type messageStore struct {
	exec        libdb.Exec
	workspaceID string
}

// NewMessageStore creates a chat-history store bound to exec, scoped to
// workspaceID.
func NewMessageStore(exec libdb.Exec, workspaceID string) MessageStore {
	if exec == nil {
		panic("SERVER BUG: runtimetypes.NewMessageStore called with nil exec")
	}
	return &messageStore{exec: exec, workspaceID: workspaceID}
}

func (s *messageStore) CreateMessageIndex(ctx context.Context, id string, identity string) error {
	_, err := s.exec.ExecContext(ctx, `
		INSERT INTO message_indices(id, identity, workspace_id)
		VALUES ($1, $2, $3)`,
		id, identity, s.workspaceID,
	)
	if err != nil {
		return fmt.Errorf("message_indices: create %q: %w", id, err)
	}
	return nil
}

func (s *messageStore) CreateNamedMessageIndex(ctx context.Context, id string, identity string, name string) error {
	_, err := s.exec.ExecContext(ctx, `
		INSERT INTO message_indices(id, identity, workspace_id, name)
		VALUES ($1, $2, $3, $4)`,
		id, identity, s.workspaceID, name,
	)
	if err != nil {
		return fmt.Errorf("message_indices: create named %q: %w", name, err)
	}
	return nil
}

func (s *messageStore) DeleteMessageIndex(ctx context.Context, id string, identity string) error {
	result, err := s.exec.ExecContext(ctx, `
		DELETE FROM message_indices
		WHERE id = $1 AND identity = $2 AND workspace_id = $3`,
		id, identity, s.workspaceID,
	)
	if err != nil {
		return fmt.Errorf("message_indices: delete %q: %w", id, err)
	}
	return checkRowsAffected(result)
}

func (s *messageStore) ListMessageIndices(ctx context.Context, identity string) ([]string, error) {
	rows, err := s.exec.QueryContext(ctx, `
		SELECT id
		FROM message_indices
		WHERE identity = $1 AND workspace_id = $2`,
		identity, s.workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("message_indices: list for %q: %w", identity, err)
	}
	defer rows.Close()

	var streams []string
	for rows.Next() {
		var stream string
		if err := rows.Scan(&stream); err != nil {
			return nil, fmt.Errorf("message_indices: scan row: %w", err)
		}
		streams = append(streams, stream)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("message_indices: rows error: %w", err)
	}
	return streams, nil
}

func (s *messageStore) ListMessageSessions(ctx context.Context, identity string) ([]MessageSession, error) {
	rows, err := s.exec.QueryContext(ctx, `
		SELECT mi.id, mi.identity, COALESCE(mi.name, ''), COUNT(m.id), MAX(m.added_at)
		FROM message_indices mi
		LEFT JOIN messages m ON m.idx_id = mi.id
		WHERE mi.identity = $1 AND mi.workspace_id = $2
		GROUP BY mi.id, mi.identity, mi.name
		ORDER BY
			CASE WHEN MAX(m.added_at) IS NULL THEN 1 ELSE 0 END ASC,
			MAX(m.added_at) DESC,
			COALESCE(mi.name, mi.id) ASC,
			mi.id ASC`,
		identity, s.workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("message_indices: list sessions for %q: %w", identity, err)
	}
	defer rows.Close()

	var sessions []MessageSession
	for rows.Next() {
		var si MessageSession
		var updatedAt any
		if err := rows.Scan(&si.ID, &si.Identity, &si.Name, &si.MessageCount, &updatedAt); err != nil {
			return nil, fmt.Errorf("message_indices: scan session: %w", err)
		}
		parsed, err := nullableMessageTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("message_indices: scan session updated_at: %w", err)
		}
		if !parsed.IsZero() {
			si.UpdatedAt = parsed.UTC()
		}
		sessions = append(sessions, si)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("message_indices: rows error: %w", err)
	}
	return sessions, nil
}

func (s *messageStore) GetMessageSessionByName(ctx context.Context, identity string, name string) (*MessageSession, error) {
	var si MessageSession
	err := s.exec.QueryRowContext(ctx, `
		SELECT id, identity, COALESCE(name, '')
		FROM message_indices
		WHERE identity = $1 AND name = $2 AND workspace_id = $3`,
		identity, name, s.workspaceID,
	).Scan(&si.ID, &si.Identity, &si.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, libdb.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("message_indices: get session %q: %w", name, err)
	}
	return &si, nil
}

func (s *messageStore) GetMessageIndexName(ctx context.Context, id string) (string, error) {
	var name string
	err := s.exec.QueryRowContext(ctx, `
		SELECT COALESCE(name, '')
		FROM message_indices
		WHERE id = $1`,
		id,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", libdb.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("message_indices: get name %q: %w", id, err)
	}
	return name, nil
}

func (s *messageStore) RenameMessageSession(ctx context.Context, id string, name string) error {
	result, err := s.exec.ExecContext(ctx, `
		UPDATE message_indices
		SET name = $2
		WHERE id = $1`,
		id,
		name,
	)
	if err != nil {
		return fmt.Errorf("message_indices: rename %q: %w", id, err)
	}
	return checkRowsAffected(result)
}

func (s *messageStore) AppendMessages(ctx context.Context, messages ...*Message) error {
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
		// UTC only: SQLite stores added_at as String() text, so a non-UTC zone would sort wrong and break every keyset boundary.
		valueArgs = append(valueArgs, msg.ID, msg.IDX, msg.Payload, msg.AddedAt.UTC())
	}

	stmt := fmt.Sprintf(`
		INSERT INTO messages (id, idx_id, payload, added_at)
		VALUES %s`,
		strings.Join(valueStrings, ","),
	)

	_, err := s.exec.ExecContext(ctx, stmt, valueArgs...)
	if err != nil {
		return fmt.Errorf("messages: append %d: %w", len(messages), err)
	}
	return nil
}

func (s *messageStore) DeleteMessages(ctx context.Context, stream string) error {
	result, err := s.exec.ExecContext(ctx, `
		DELETE FROM messages
		WHERE idx_id = $1`,
		stream,
	)
	if err != nil {
		return fmt.Errorf("messages: delete for %q: %w", stream, err)
	}
	return checkRowsAffected(result)
}

func (s *messageStore) ListMessages(ctx context.Context, stream string) ([]*Message, error) {
	rows, err := s.exec.QueryContext(ctx, `
		SELECT id, idx_id, payload, added_at
		FROM messages
		WHERE idx_id = $1
		ORDER BY added_at ASC`,
		stream,
	)
	if err != nil {
		return nil, fmt.Errorf("messages: list for %q: %w", stream, err)
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.IDX, &msg.Payload, &msg.AddedAt); err != nil {
			return nil, fmt.Errorf("messages: scan row: %w", err)
		}
		msgs = append(msgs, &msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("messages: rows error: %w", err)
	}
	return msgs, nil
}

func (s *messageStore) ListMessagesPage(ctx context.Context, stream string, f MessagePageFilter) ([]*Message, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultMessagePageLimit
	}
	if limit > MaxMessagePageLimit {
		limit = MaxMessagePageLimit
	}
	cmp, order := ">", "ASC"
	if f.Backwards {
		cmp, order = "<", "DESC"
	}
	// (added_at, id) > (cursor) is spelled out by hand: SQLite has no row-value comparison syntax.
	args := []any{stream}
	where := "idx_id = $1"
	if !f.After.AddedAt.IsZero() {
		args = append(args, f.After.AddedAt.UTC(), f.After.ID)
		where += fmt.Sprintf(" AND (added_at %s $%d OR (added_at = $%d AND id %s $%d))",
			cmp, len(args)-1, len(args)-1, cmp, len(args))
	}
	args = append(args, limit)
	rows, err := s.exec.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, idx_id, payload, added_at
		FROM messages
		WHERE %s
		ORDER BY added_at %s, id %s
		LIMIT $%d`, where, order, order, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("messages: page for %q: %w", stream, err)
	}
	defer rows.Close()

	msgs := []*Message{}
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.IDX, &msg.Payload, &msg.AddedAt); err != nil {
			return nil, fmt.Errorf("messages: scan row: %w", err)
		}
		msgs = append(msgs, &msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("messages: rows error: %w", err)
	}
	return msgs, nil
}

func (s *messageStore) LastMessage(ctx context.Context, stream string) (*Message, error) {
	row := s.exec.QueryRowContext(ctx, `
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
			return nil, libdb.ErrNotFound
		}
		return nil, fmt.Errorf("messages: last for %q: %w", stream, err)
	}
	return &msg, nil
}

func (s *messageStore) CountMessages(ctx context.Context, stream string) (int, error) {
	var count int
	err := s.exec.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM messages
		WHERE idx_id = $1`,
		stream,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("messages: count for %q: %w", stream, err)
	}
	return count, nil
}

// ListAllMessageIndices returns every index row in the database, in no particular order, each with its message count.
func ListAllMessageIndices(ctx context.Context, exec libdb.Exec) ([]MessageIndexRow, error) {
	rows, err := exec.QueryContext(ctx, `
		SELECT mi.id, mi.identity, mi.workspace_id, COALESCE(mi.name, ''),
		       (SELECT COUNT(*) FROM messages m WHERE m.idx_id = mi.id)
		FROM message_indices mi`)
	if err != nil {
		return nil, fmt.Errorf("message_indices: list all: %w", err)
	}
	defer rows.Close()

	var out []MessageIndexRow
	for rows.Next() {
		var r MessageIndexRow
		if err := rows.Scan(&r.ID, &r.Identity, &r.WorkspaceID, &r.Name, &r.MessageCount); err != nil {
			return nil, fmt.Errorf("message_indices: scan row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("message_indices: rows error: %w", err)
	}
	return out, nil
}

// ResolveMessageIndexWorkspace returns the workspace owning identity's index named name, preferring the busiest then highest id when several match, or libdb.ErrNotFound when none does.
func ResolveMessageIndexWorkspace(ctx context.Context, exec libdb.Exec, identity string, name string) (string, error) {
	var workspaceID string
	err := exec.QueryRowContext(ctx, `
		SELECT mi.workspace_id
		FROM message_indices mi
		WHERE mi.name = $1 AND mi.identity = $2
		ORDER BY (SELECT COUNT(*) FROM messages m WHERE m.idx_id = mi.id) DESC, mi.id DESC
		LIMIT 1`,
		name, identity,
	).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", libdb.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("message_indices: resolve workspace for %q: %w", name, err)
	}
	return workspaceID, nil
}

func nullableMessageTime(value any) (time.Time, error) {
	switch v := value.(type) {
	case nil:
		return time.Time{}, nil
	case time.Time:
		return v, nil
	case string:
		return parseMessageTimeString(v)
	case []byte:
		return parseMessageTimeString(string(v))
	default:
		return time.Time{}, fmt.Errorf("unsupported time value %T", value)
	}
}

func parseMessageTimeString(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q", value)
}
