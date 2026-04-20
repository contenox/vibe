// Package terminalservice runs a real shell session on the local machine. Chains call into this to execute commands and read back real output.
package terminalservice

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/terminalstore"
)

// CreateRequest is input for an interactive shell session.
type CreateRequest struct {
	CWD   string
	Cols  int
	Rows  int
	Shell string // optional; defaults from config
}

// CreateResponse is returned after a session is allocated.
type CreateResponse struct {
	ID string
}

// SessionInfo is persisted metadata for a terminal session (from DB).
type SessionInfo = terminalstore.Session

// Service manages PTY-backed terminal sessions.
type Service interface {
	Create(ctx context.Context, principal string, req CreateRequest) (*CreateResponse, error)
	Close(ctx context.Context, principal, id string) error
	CloseAll(ctx context.Context) error
	Attach(ctx context.Context, principal, id string, conn io.ReadWriteCloser, resizeCh <-chan ResizeMsg) error
	Get(ctx context.Context, principal, id string) (*SessionInfo, error)
	List(ctx context.Context, principal string, createdAtCursor *time.Time, limit int) ([]*SessionInfo, error)
	UpdateGeometry(ctx context.Context, principal, id string, cols, rows int) error
	// ReapIdle closes the session if it has been detached longer than [Config.IdleTimeout].
	// A zero IdleTimeout disables reaping.
	ReapIdle(ctx context.Context) error
}

// ResizeMsg carries terminal dimension changes from the WebSocket handler.
type ResizeMsg struct {
	Cols int
	Rows int
}

type service struct {
	cfg            Config
	db             libdb.DBManager
	nodeInstanceID string
	current        atomic.Pointer[session] // active session, or nil
}

// New constructs a terminal [Service]. When cfg.Enabled is false, returns [NewDisabled] with a nil error.
// db may be nil only when cfg.Enabled is false.
// Build cfg with [ParseEnv].
func New(cfg Config, db libdb.DBManager, nodeInstanceID string) (Service, error) {
	if !cfg.Enabled {
		return NewDisabled(), nil
	}
	if db == nil {
		return nil, errors.New("terminalservice: database is required when terminal is enabled")
	}
	s := &service{
		cfg:            cfg,
		db:             db,
		nodeInstanceID: nodeInstanceID,
	}
	st := terminalstore.New(s.db.WithoutTransaction())
	// Remove orphaned rows from a previous process on this node (PTY state is gone).
	if err := st.DeleteByNodeInstanceID(context.Background(), s.nodeInstanceID); err != nil {
		return nil, err
	}
	return s, nil
}

// NewDisabled returns a service that rejects mutating operations with [ErrDisabled] (feature off).
func NewDisabled() Service {
	return disabledService{}
}

type disabledService struct{}

func (disabledService) Create(context.Context, string, CreateRequest) (*CreateResponse, error) {
	return nil, ErrDisabled
}
func (disabledService) Close(context.Context, string, string) error { return ErrDisabled }
func (disabledService) CloseAll(context.Context) error              { return ErrDisabled }
func (disabledService) Attach(context.Context, string, string, io.ReadWriteCloser, <-chan ResizeMsg) error {
	return ErrDisabled
}
func (disabledService) Get(context.Context, string, string) (*SessionInfo, error) {
	return nil, ErrDisabled
}
func (disabledService) List(context.Context, string, *time.Time, int) ([]*SessionInfo, error) {
	return nil, ErrDisabled
}
func (disabledService) UpdateGeometry(context.Context, string, string, int, int) error {
	return ErrDisabled
}
func (disabledService) ReapIdle(context.Context) error { return nil }

func (s *service) store() terminalstore.Store {
	return terminalstore.New(s.db.WithoutTransaction())
}

// localByID returns the active session if its id matches.
func (s *service) localByID(id string) *session {
	sess := s.current.Load()
	if sess == nil || sess.id != id {
		return nil
	}
	return sess
}

func (s *service) Get(ctx context.Context, principal, id string) (*SessionInfo, error) {
	row, err := s.store().GetByIDAndPrincipal(ctx, id, principal)
	if err != nil {
		if errors.Is(err, terminalstore.ErrNotFound) {
			return nil, ErrSessionNotFound
		}
		return nil, err
	}
	return row, nil
}

func (s *service) List(ctx context.Context, principal string, createdAtCursor *time.Time, limit int) ([]*SessionInfo, error) {
	return s.store().ListByPrincipal(ctx, principal, createdAtCursor, limit)
}

func (s *service) UpdateGeometry(ctx context.Context, principal, id string, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return apiframework.BadRequest("cols and rows must be positive")
	}
	st := s.store()
	row, err := st.GetByIDAndPrincipal(ctx, id, principal)
	if err != nil {
		if errors.Is(err, terminalstore.ErrNotFound) {
			return ErrSessionNotFound
		}
		return err
	}
	if row.Status != terminalstore.SessionStatusActive {
		return ErrSessionNotFound
	}
	if err := st.UpdateGeometry(ctx, id, cols, rows); err != nil {
		if errors.Is(err, terminalstore.ErrNotFound) {
			return ErrSessionNotFound
		}
		return err
	}
	s.resizeLocalPTY(id, cols, rows)
	return nil
}

func (s *service) Close(ctx context.Context, principal, id string) error {
	st := s.store()
	row, err := st.GetByIDAndPrincipal(ctx, id, principal)
	if err != nil {
		if errors.Is(err, terminalstore.ErrNotFound) {
			return ErrSessionNotFound
		}
		return err
	}
	if row.Status != terminalstore.SessionStatusActive {
		return ErrSessionNotFound
	}
	return s.closeByID(ctx, id)
}

// closeByID releases the active session and deletes its row. Idempotent.
func (s *service) closeByID(ctx context.Context, id string) error {
	if sess := s.current.Load(); sess != nil && sess.id == id {
		if s.current.CompareAndSwap(sess, nil) {
			_ = sess.shutdown(ctx)
		}
	}
	if err := s.store().Delete(ctx, id); err != nil {
		if errors.Is(err, terminalstore.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (s *service) ReapIdle(ctx context.Context) error {
	if s.cfg.IdleTimeout <= 0 {
		return nil
	}
	sess := s.current.Load()
	if sess == nil {
		return nil
	}
	if time.Since(sess.lastActivity()) < s.cfg.IdleTimeout {
		return nil
	}
	// Claim busy before clearing the slot so a concurrent Attach either wins
	// the CAS first (and the reap is a no-op) or finds an empty slot.
	if !sess.busy.CompareAndSwap(false, true) {
		return nil
	}
	if !s.current.CompareAndSwap(sess, nil) {
		sess.busy.Store(false)
		return nil
	}
	_ = sess.shutdown(ctx)
	if err := s.store().Delete(ctx, sess.id); err != nil && !errors.Is(err, terminalstore.ErrNotFound) {
		return err
	}
	return nil
}

func (s *service) CloseAll(ctx context.Context) error {
	if sess := s.current.Swap(nil); sess != nil {
		_ = sess.shutdown(ctx)
	}
	st := terminalstore.New(s.db.WithoutTransaction())
	return st.DeleteByNodeInstanceID(ctx, s.nodeInstanceID)
}
