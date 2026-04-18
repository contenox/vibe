package modelregistryservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtimetypes"
)

var ErrInvalidEntry = errors.New("invalid model registry entry")

type Service interface {
	Create(ctx context.Context, e *runtimetypes.ModelRegistryEntry) error
	Get(ctx context.Context, id string) (*runtimetypes.ModelRegistryEntry, error)
	GetByName(ctx context.Context, name string) (*runtimetypes.ModelRegistryEntry, error)
	Update(ctx context.Context, e *runtimetypes.ModelRegistryEntry) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, cursor *time.Time, limit int) ([]*runtimetypes.ModelRegistryEntry, error)
}

type service struct {
	dbInstance libdb.DBManager
}

func New(db libdb.DBManager) Service {
	return &service{dbInstance: db}
}

func validate(e *runtimetypes.ModelRegistryEntry) error {
	if e.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidEntry)
	}
	if e.SourceURL == "" {
		return fmt.Errorf("%w: sourceUrl is required", ErrInvalidEntry)
	}
	return nil
}

func (s *service) Create(ctx context.Context, e *runtimetypes.ModelRegistryEntry) error {
	if err := validate(e); err != nil {
		return err
	}
	tx := s.dbInstance.WithoutTransaction()
	st := runtimetypes.New(tx)
	count, err := st.EstimateModelRegistryEntryCount(ctx)
	if err != nil {
		return err
	}
	if err := st.EnforceMaxRowCount(ctx, count); err != nil {
		return fmt.Errorf("too many rows in the system: %w", err)
	}
	return st.CreateModelRegistryEntry(ctx, e)
}

func (s *service) Get(ctx context.Context, id string) (*runtimetypes.ModelRegistryEntry, error) {
	return runtimetypes.New(s.dbInstance.WithoutTransaction()).GetModelRegistryEntry(ctx, id)
}

func (s *service) GetByName(ctx context.Context, name string) (*runtimetypes.ModelRegistryEntry, error) {
	return runtimetypes.New(s.dbInstance.WithoutTransaction()).GetModelRegistryEntryByName(ctx, name)
}

func (s *service) Update(ctx context.Context, e *runtimetypes.ModelRegistryEntry) error {
	if err := validate(e); err != nil {
		return err
	}
	return runtimetypes.New(s.dbInstance.WithoutTransaction()).UpdateModelRegistryEntry(ctx, e)
}

func (s *service) Delete(ctx context.Context, id string) error {
	return runtimetypes.New(s.dbInstance.WithoutTransaction()).DeleteModelRegistryEntry(ctx, id)
}

func (s *service) List(ctx context.Context, cursor *time.Time, limit int) ([]*runtimetypes.ModelRegistryEntry, error) {
	return runtimetypes.New(s.dbInstance.WithoutTransaction()).ListModelRegistryEntries(ctx, cursor, limit)
}
