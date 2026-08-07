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

// Durable chat history over message_indices and messages: an index row is one
// conversation (a CLI/ACP session), and every message row hangs off it by
// idx_id under ON DELETE CASCADE, so dropping the index drops the thread.
//
// Index rows are WORKSPACE-scoped: workspace_id is fixed at construction, never
// a filter argument, so no caller can widen a read past its own workspace.
// Message rows carry no workspace_id of their own — they inherit it through the
// idx_id foreign key, which is why the message-level methods take a stream ID
// and no workspace.
//
// Payload is opaque to this layer: the service that writes it owns its shape
// (taskengine.Message JSON today), and the store never parses it.

// Message-page bounds. A caller that names no limit gets
// DefaultMessagePageLimit; one that asks for more than MaxMessagePageLimit is
// clamped, so no caller can turn a page read back into a whole-session read.
const (
	DefaultMessagePageLimit = 100
	MaxMessagePageLimit     = 1000
)

// MessageStore is chat-history persistence, scoped to one workspace at
// construction.
//
// It takes an Exec rather than a DBManager, like most of this package's stores:
// every method is a single statement, and callers that need several to commit
// together (sessionservice's create-and-activate, chatservice's persist path)
// pass their own transaction's Exec in.
type MessageStore interface {
	// CreateMessageIndex creates an unnamed conversation index for identity.
	CreateMessageIndex(ctx context.Context, id string, identity string) error
	// CreateNamedMessageIndex creates a named conversation index. The name is
	// unique per workspace (idx_message_indices_name), so a duplicate is a
	// constraint error, not a silent second session.
	CreateNamedMessageIndex(ctx context.Context, id string, identity string, name string) error
	// DeleteMessageIndex removes identity's index, cascading to its messages.
	// Returns libdb.ErrNotFound when no row matched.
	DeleteMessageIndex(ctx context.Context, id string, identity string) error
	// ListMessageIndices returns identity's index IDs in this workspace.
	ListMessageIndices(ctx context.Context, identity string) ([]string, error)
	// ListMessageSessions returns identity's indices with their message counts
	// and last-activity times, most recently active first; never-used sessions
	// sort last, then by name and ID.
	ListMessageSessions(ctx context.Context, identity string) ([]MessageSession, error)
	// GetMessageSessionByName returns identity's session with that name.
	// Returns libdb.ErrNotFound when none exists.
	GetMessageSessionByName(ctx context.Context, identity string, name string) (*MessageSession, error)
	// GetMessageIndexName returns an index's human-readable name by ID, "" when
	// the row exists but carries none. Returns libdb.ErrNotFound when no row
	// matched. Keyed on the primary key alone, for the same reason
	// RenameMessageSession is.
	GetMessageIndexName(ctx context.Context, id string) (string, error)
	// RenameMessageSession sets a session's human-readable name. Returns
	// libdb.ErrNotFound when no row matched.
	RenameMessageSession(ctx context.Context, id string, name string) error

	// AppendMessages inserts messages in one batch statement. A zero AddedAt is
	// stamped with now (UTC) on the passed struct. Empty input is a no-op.
	AppendMessages(ctx context.Context, messages ...*Message) error
	// DeleteMessages removes every message of stream, keeping the index row.
	// Returns libdb.ErrNotFound when the stream held no messages.
	DeleteMessages(ctx context.Context, stream string) error
	// ListMessages returns stream's messages oldest first.
	ListMessages(ctx context.Context, stream string) ([]*Message, error)
	// ListMessagesPage returns one keyset page of stream's messages in
	// (added_at, id) order — the read to use instead of ListMessages when the
	// caller wants a bounded prefix rather than a whole conversation.
	//
	// Keyset, not OFFSET: every page resumes from the previous page's last row
	// (Message.Cursor), so a row appended between two page reads can only land
	// past the boundary, never shift it and make a page drop or repeat a row.
	// A page shorter than the effective limit is the end of the stream.
	ListMessagesPage(ctx context.Context, stream string, f MessagePageFilter) ([]*Message, error)
	// LastMessage returns stream's newest message, libdb.ErrNotFound when empty.
	LastMessage(ctx context.Context, stream string) (*Message, error)
	// CountMessages returns stream's message count.
	CountMessages(ctx context.Context, stream string) (int, error)
}

// Message is one stored conversation turn. IDX names its index (stream), and
// (ID, IDX) is the primary key — re-appending the same ID to the same stream is
// a constraint error, which is what makes the service layer's diff-append safe.
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

// MessageCursor is one page boundary: the (added_at, id) of the last row a
// page returned.
//
// ID is not decoration. added_at is NOT unique — one AppendMessages batch
// stamps every zero-timestamped message the same instant — so a boundary
// expressed as a timestamp alone lands in the middle of a tie, and the next
// page either re-reads the tied rows or skips them. (added_at, id) is total:
// (id, idx_id) is the messages primary key, so within one stream id is unique.
type MessageCursor struct {
	AddedAt time.Time
	ID      string
}

// MessagePageFilter narrows a ListMessagesPage read. The zero value is the
// stream's first page, oldest first, of DefaultMessagePageLimit rows. The
// stream is never a filter field and the workspace is never either — one is a
// method argument, the other fixed by the store.
type MessagePageFilter struct {
	// After resumes strictly after this boundary, or strictly before it when
	// Backwards. A zero AddedAt starts at the stream's oldest (newest) message.
	After MessageCursor
	// Backwards walks the stream newest first.
	Backwards bool
	// Limit caps the page: <= 0 means DefaultMessagePageLimit, more than
	// MaxMessagePageLimit is clamped to it.
	Limit int
}

// MessageSession is one conversation index row plus its aggregates. Name is
// empty for an unnamed index; UpdatedAt is zero for one that holds no messages.
type MessageSession struct {
	ID           string
	Identity     string
	Name         string
	MessageCount int
	UpdatedAt    time.Time
}

// MessageIndexRow is one index row with its message count, read across every
// workspace at once — what operator tooling needs when it must report what the
// database holds rather than what one workspace can see.
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

// GetMessageIndexName keys on the primary key alone, for RenameMessageSession's
// reason: session IDs are UUIDs, unique across workspaces. Callers hold an
// internal ID they were already given; the lookup only turns it back into the
// name the ACP/CLI surfaces address a session by.
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

// RenameMessageSession keys on the primary key alone: session IDs are UUIDs,
// unique across workspaces, so the workspace predicate the other index
// statements carry would add nothing here.
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
		// Bound as UTC on purpose: SQLite keeps added_at as the TEXT of the
		// bound time's String(), so a value carrying a non-UTC zone would sort
		// and compare by its local wall clock — breaking ORDER BY added_at and
		// every keyset boundary in ListMessagesPage.
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
	// The stream predicate is placeholder $1 and unconditional; the boundary
	// appends after it so $N always matches len(args). The boundary is the
	// row-comparison (added_at, id) > (cursor) spelled out, because SQLite has
	// no row-value comparison and Postgres and it disagree on the sugar.
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

// ListAllMessageIndices returns every index row in the database, in no
// particular order, each with its message count.
//
// Deliberately a function and not a MessageStore method: MessageStore fixes one
// workspace at construction so that no read can widen past it, and this read is
// whole-database BY INTENT — the CLI's cross-workspace session inventory, whose
// entire job is showing rows the active workspace cannot see. Keeping it off the
// interface is what keeps that intent visible at the call site.
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

// ResolveMessageIndexWorkspace returns the workspace owning identity's index
// named name. The unique index on (name, workspace_id) makes the name unique
// only WITHIN a workspace, so the same session name can exist in several: the
// busiest one wins, then the highest id, so the answer is stable.
// Returns libdb.ErrNotFound when no index carries that name.
//
// Deliberately a function and not a MessageStore method: it answers WHICH
// workspace a session belongs to, so it is the call that picks a store's
// workspace and cannot itself be scoped to one.
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

// nullableMessageTime decodes MAX(added_at) from the aggregate read, where the
// driver may hand back nil (no messages), a time, or a formatted string.
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

// parseMessageTimeString tries the layouts SQLite TIMESTAMP columns come back
// in, widest first.
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
