package backendservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

var ErrInvalidBackend = errors.New("invalid backend data")

type Service interface {
	Create(ctx context.Context, backend *runtimetypes.Backend) error
	Get(ctx context.Context, id string) (*runtimetypes.Backend, error)
	Update(ctx context.Context, backend *runtimetypes.Backend) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Backend, error)
}

type service struct {
	dbInstance libdb.DBManager
}

type duplicateBackendError struct {
	typ     string
	baseURL string
	cause   error
}

func (e duplicateBackendError) Error() string {
	return fmt.Sprintf("backend already exists for type %q and base URL %q", e.typ, e.baseURL)
}

func (e duplicateBackendError) Unwrap() error {
	return e.cause
}

func New(db libdb.DBManager) Service {
	return &service{dbInstance: db}
}

func (s *service) Create(ctx context.Context, backend *runtimetypes.Backend) error {
	tx := s.dbInstance.WithoutTransaction()
	if err := validate(backend); err != nil {
		return err
	}
	storeInstance := runtimetypes.New(tx)
	count, err := storeInstance.EstimateBackendCount(ctx)
	if err != nil {
		return err
	}
	if err := storeInstance.EnforceMaxRowCount(ctx, count); err != nil {
		return fmt.Errorf("too many rows in the system (current %d): %w", count, err)
	}

	if err := storeInstance.CreateBackend(ctx, backend); err != nil {
		return sanitizeBackendStoreError(backend, err)
	}
	return nil
}

func (s *service) Get(ctx context.Context, id string) (*runtimetypes.Backend, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).GetBackend(ctx, id)
}

func (s *service) Update(ctx context.Context, backend *runtimetypes.Backend) error {
	if err := validate(backend); err != nil {
		return err
	}
	tx := s.dbInstance.WithoutTransaction()
	if err := runtimetypes.New(tx).UpdateBackend(ctx, backend); err != nil {
		return sanitizeBackendStoreError(backend, err)
	}
	return nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).DeleteBackend(ctx, id)
}

func (s *service) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Backend, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).ListBackends(ctx, createdAtCursor, limit)
}

func validate(backend *runtimetypes.Backend) error {
	if backend.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidBackend)
	}
	if backend.BaseURL == "" {
		return fmt.Errorf("%w: baseURL is required", ErrInvalidBackend)
	}
	switch modelrepo.CanonicalBackendType(backend.Type) {
	case "ollama", "vllm", "openai", "anthropic", "bedrock", "gemini", "vertex-google":
	default:
		return fmt.Errorf("%w: Type must be ollama, vllm, openai, anthropic, bedrock, gemini, or vertex-google", ErrInvalidBackend)
	}

	return nil
}

func sanitizeBackendStoreError(backend *runtimetypes.Backend, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, libdb.ErrUniqueViolation) && backend != nil {
		return duplicateBackendError{
			typ:     backend.Type,
			baseURL: backend.BaseURL,
			cause:   err,
		}
	}
	return err
}
