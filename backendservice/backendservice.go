package backendservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
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
		err := fmt.Errorf("too many rows in the system: %w", err)
		fmt.Printf("SERVER ERROR: creation blocked: limit reached current %d %v", count, err)
		return err
	}

	return storeInstance.CreateBackend(ctx, backend)
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
	return runtimetypes.New(tx).UpdateBackend(ctx, backend)
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
	if backend.Type != "ollama" && backend.Type != "vllm" {
		return fmt.Errorf("%w: Type is required to be ollama or vllm", ErrInvalidBackend)
	}

	return nil
}
