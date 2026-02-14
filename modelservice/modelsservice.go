package modelservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/vibe/apiframework"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
)

var ErrInvalidModel = errors.New("invalid model data")

type service struct {
	dbInstance              libdb.DBManager
	immutableEmbedModelName string
}

type Service interface {
	Append(ctx context.Context, model *runtimetypes.Model) error
	Update(ctx context.Context, data *runtimetypes.Model) error
	List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Model, error)
	Delete(ctx context.Context, modelName string) error
}

func New(db libdb.DBManager, embedModel string) Service {
	return &service{
		dbInstance:              db,
		immutableEmbedModelName: embedModel,
	}
}

func (s *service) Append(ctx context.Context, model *runtimetypes.Model) error {

	if err := validate(model); err != nil {
		return err
	}
	tx := s.dbInstance.WithoutTransaction()
	storeInstance := runtimetypes.New(tx)
	count, err := storeInstance.EstimateModelCount(ctx)
	if err != nil {
		return err
	}
	err = storeInstance.EnforceMaxRowCount(ctx, count)
	if err != nil {
		return err
	}
	return storeInstance.AppendModel(ctx, model)
}

func (s *service) Update(ctx context.Context, data *runtimetypes.Model) error {

	if err := validate(data); err != nil {
		return err
	}
	if data.ID == "" {
		return fmt.Errorf("%w %w: id is required", apiframework.ErrBadRequest, ErrInvalidModel)
	}
	tx := s.dbInstance.WithoutTransaction()
	storeInstance := runtimetypes.New(tx)

	return storeInstance.UpdateModel(ctx, data)
}

func (s *service) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Model, error) {
	tx := s.dbInstance.WithoutTransaction()
	return runtimetypes.New(tx).ListModels(ctx, createdAtCursor, limit)
}

func (s *service) Delete(ctx context.Context, modelName string) error {
	tx := s.dbInstance.WithoutTransaction()
	if modelName == s.immutableEmbedModelName {
		return apiframework.ErrImmutableModel
	}
	return runtimetypes.New(tx).DeleteModel(ctx, modelName)
}

func validate(model *runtimetypes.Model) error {
	if model.Model == "" {
		return fmt.Errorf("%w %w: model name is required", apiframework.ErrBadRequest, ErrInvalidModel)
	}
	if model.ContextLength == 0 {
		return fmt.Errorf("%w %w: context length is required", apiframework.ErrBadRequest, ErrInvalidModel)
	}
	if !model.CanChat && !model.CanEmbed && !model.CanPrompt && !model.CanStream {
		return fmt.Errorf("%w %w: capabilities are required", apiframework.ErrBadRequest, ErrInvalidModel)
	}
	return nil
}

func (s *service) GetServiceName() string {
	return "modelservice"
}
