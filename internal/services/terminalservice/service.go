// Package terminalservice manages local PTY-backed shell sessions.
package terminalservice

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/contenox/beam/internal/errdefs"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/terminalstore"
)

type CreateRequest struct {
	CWD   string
	Cols  int
	Rows  int
	Shell string
}

type CreateResponse struct {
	ID string
}

type SessionInfo = terminalstore.Session

type Service interface {
	Create(ctx context.Context, principal string, req CreateRequest) (*CreateResponse, error)
	Close(ctx context.Context, principal, id string) error
	CloseAll(ctx context.Context) error
	Attach(ctx context.Context, principal, id string, conn io.ReadWriteCloser, resizeCh <-chan ResizeMsg) error
	Get(ctx context.Context, principal, id string) (*SessionInfo, error)
	List(ctx context.Context, principal string, createdAtCursor *time.Time, limit int) ([]*SessionInfo, error)
	UpdateGeometry(ctx context.Context, principal, id string, cols, rows int) error
	ReapIdle(ctx context.Context) error
}

type ResizeMsg struct {
	Cols int
	Rows int
}

type service struct {
	cfg            Config
	db             libdb.DBManager
	nodeInstanceID string
	workspaceID    string
	tracker        libtracker.ActivityTracker
	sessions       sync.Map // id -> *session
}

// Option configures the terminal service at construction, so its optional
// dependencies can be wired without changing New's signature.
type Option func(*service)

// WithTracker wires the ActivityTracker the pty plumbing reports to: the
// stream pumps behind an attachment and the local resize, none of which can
// return an error to a caller (they run on goroutines the caller never sees, or
// after the durable geometry write already succeeded). It is distinct from
// WithActivityTracker, which instruments the Service API from outside; this one
// reaches the events that never cross that boundary. Nil degrades to
// libtracker.NoopTracker.
func WithTracker(tracker libtracker.ActivityTracker) Option {
	return func(s *service) {
		if tracker != nil {
			s.tracker = tracker
		}
	}
}

func New(cfg Config, db libdb.DBManager, nodeInstanceID string, workspaceID string, opts ...Option) (Service, error) {
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
		workspaceID:    workspaceID,
		tracker:        libtracker.NoopTracker{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if s.tracker == nil {
		s.tracker = libtracker.NoopTracker{}
	}
	if err := terminalstore.InitSchema(context.Background(), s.db.WithoutTransaction()); err != nil {
		return nil, err
	}
	st := terminalstore.New(s.db.WithoutTransaction(), s.workspaceID)
	if err := st.DeleteByNodeInstanceID(context.Background(), s.nodeInstanceID); err != nil {
		return nil, err
	}
	return s, nil
}

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
	return terminalstore.New(s.db.WithoutTransaction(), s.workspaceID)
}

func (s *service) putSession(sess *session) {
	s.sessions.Store(sess.id, sess)
}

func (s *service) removeSession(id string) *session {
	v, ok := s.sessions.LoadAndDelete(id)
	if !ok {
		return nil
	}
	return v.(*session)
}

func (s *service) sessionCount() int {
	n := 0
	s.sessions.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

func (s *service) forEachSession(fn func(*session) error) error {
	var firstErr error
	s.sessions.Range(func(_, value any) bool {
		if err := fn(value.(*session)); err != nil && firstErr == nil {
			firstErr = err
		}
		return true
	})
	return firstErr
}

func (s *service) localByID(id string) *session {
	v, ok := s.sessions.Load(id)
	if !ok {
		return nil
	}
	return v.(*session)
}

func (s *service) atSessionCapacity() bool {
	max := s.cfg.MaxSessions
	if max <= 0 {
		return false
	}
	return s.sessionCount() >= max
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
		return errdefs.BadRequest("cols and rows must be positive")
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
	s.resizeLocalPTY(ctx, id, cols, rows)
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

func (s *service) closeByID(ctx context.Context, id string) error {
	if sess := s.removeSession(id); sess != nil {
		_ = sess.shutdown(ctx)
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
	var firstErr error
	s.sessions.Range(func(key, value any) bool {
		sess := value.(*session)
		if time.Since(sess.lastActivity()) < s.cfg.IdleTimeout {
			return true
		}
		if !sess.busy.CompareAndSwap(false, true) {
			return true
		}
		id := sess.id
		removed := s.removeSession(id)
		if removed == nil {
			sess.busy.Store(false)
			return true
		}
		_ = removed.shutdown(ctx)
		if err := s.store().Delete(ctx, id); err != nil && !errors.Is(err, terminalstore.ErrNotFound) && firstErr == nil {
			firstErr = err
		}
		return true
	})
	return firstErr
}

func (s *service) CloseAll(ctx context.Context) error {
	s.sessions.Range(func(key, value any) bool {
		sess := value.(*session)
		s.sessions.Delete(key)
		_ = sess.shutdown(ctx)
		return true
	})
	st := terminalstore.New(s.db.WithoutTransaction(), s.workspaceID)
	return st.DeleteByNodeInstanceID(ctx, s.nodeInstanceID)
}
