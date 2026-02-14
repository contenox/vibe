package affinitygroupservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/internal/runtimestate"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/google/uuid"
)

var (
	ErrInvalidAffinityGroup = errors.New("invalid affinity group data")
	ErrNotFound             = libdb.ErrNotFound
)

type service struct {
	dbInstance libdb.DBManager
}

func New(db libdb.DBManager) Service {
	return &service{dbInstance: db}
}

type Service interface {
	Create(ctx context.Context, group *runtimetypes.AffinityGroup) error
	GetByID(ctx context.Context, id string) (*runtimetypes.AffinityGroup, error)
	GetByName(ctx context.Context, name string) (*runtimetypes.AffinityGroup, error)
	Update(ctx context.Context, group *runtimetypes.AffinityGroup) error
	Delete(ctx context.Context, id string) error
	ListAll(ctx context.Context) ([]*runtimetypes.AffinityGroup, error)
	ListByPurpose(ctx context.Context, purpose string, createdAtCursor *time.Time, limit int) ([]*runtimetypes.AffinityGroup, error)
	AssignBackend(ctx context.Context, groupID, backendID string) error
	RemoveBackend(ctx context.Context, groupID, backendID string) error
	ListBackends(ctx context.Context, groupID string) ([]*runtimetypes.Backend, error)
	ListAffinityGroupsForBackend(ctx context.Context, backendID string) ([]*runtimetypes.AffinityGroup, error)
	AssignModel(ctx context.Context, groupID, modelID string) error
	RemoveModel(ctx context.Context, groupID, modelID string) error
	ListModels(ctx context.Context, groupID string) ([]*runtimetypes.Model, error)
	ListAffinityGroupsForModel(ctx context.Context, modelID string) ([]*runtimetypes.AffinityGroup, error)
}

func (s *service) Create(ctx context.Context, group *runtimetypes.AffinityGroup) error {
	group.ID = uuid.New().String()
	tx := s.dbInstance.WithoutTransaction()
	storeInstance := runtimetypes.New(tx)
	count, err := storeInstance.EstimateAffinityGroupCount(ctx)
	if err != nil {
		return err
	}
	err = storeInstance.EnforceMaxRowCount(ctx, count)
	if err != nil {
		return err
	}
	return storeInstance.CreateAffinityGroup(ctx, group)
}

func (s *service) GetByID(ctx context.Context, id string) (*runtimetypes.AffinityGroup, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).GetAffinityGroup(ctx, id)
}

func (s *service) GetByName(ctx context.Context, name string) (*runtimetypes.AffinityGroup, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).GetAffinityGroupByName(ctx, name)
}

func (s *service) Update(ctx context.Context, group *runtimetypes.AffinityGroup) error {
	if group.ID == runtimestate.EmbedgroupID {
		return fmt.Errorf("affinity group %s is immutable", group.ID)
	}
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).UpdateAffinityGroup(ctx, group)
}

func (s *service) Delete(ctx context.Context, id string) error {
	if id == runtimestate.EmbedgroupID {
		return fmt.Errorf("affinity group %s is immutable", id)
	}
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).DeleteAffinityGroup(ctx, id)
}

func (s *service) ListAll(ctx context.Context) ([]*runtimetypes.AffinityGroup, error) {
	tx := s.dbInstance.WithoutTransaction()
	storeInstance := runtimetypes.New(tx)

	var allgroups []*runtimetypes.AffinityGroup
	var lastCursor *time.Time
	limit := 100 // A reasonable page size

	for {
		page, err := storeInstance.ListAffinityGroups(ctx, lastCursor, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to list groups: %w", err)
		}

		allgroups = append(allgroups, page...)

		if len(page) < limit {
			break // No more pages
		}

		lastCursor = &page[len(page)-1].CreatedAt
	}

	return allgroups, nil
}

func (s *service) ListByPurpose(ctx context.Context, purpose string, createdAtCursor *time.Time, limit int) ([]*runtimetypes.AffinityGroup, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).ListAffinityGroupByPurpose(ctx, purpose, createdAtCursor, limit)
}

func (s *service) AssignBackend(ctx context.Context, groupID, backendID string) error {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).AssignBackendToAffinityGroup(ctx, groupID, backendID)
}

func (s *service) RemoveBackend(ctx context.Context, groupID, backendID string) error {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).RemoveBackendFromAffinityGroup(ctx, groupID, backendID)
}

func (s *service) ListBackends(ctx context.Context, groupID string) ([]*runtimetypes.Backend, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).ListBackendsForAffinityGroup(ctx, groupID)
}

func (s *service) ListAffinityGroupsForBackend(ctx context.Context, backendID string) ([]*runtimetypes.AffinityGroup, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).ListAffinityGroupsForBackend(ctx, backendID)
}

func (s *service) AssignModel(ctx context.Context, groupID, modelID string) error {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).AssignModelToAffinityGroup(ctx, groupID, modelID)
}

func (s *service) RemoveModel(ctx context.Context, groupID, modelID string) error {
	if groupID == runtimestate.EmbedgroupID {
		return apiframework.ErrImmutableGroup
	}
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).RemoveModelFromAffinityGroup(ctx, groupID, modelID)
}

func (s *service) ListModels(ctx context.Context, groupID string) ([]*runtimetypes.Model, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).ListModelsForAffinityGroup(ctx, groupID)
}

func (s *service) ListAffinityGroupsForModel(ctx context.Context, modelID string) ([]*runtimetypes.AffinityGroup, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).ListAffinityGroupsForModel(ctx, modelID)
}
