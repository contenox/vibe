package functionservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/vibe/functionstore"
	libdb "github.com/contenox/vibe/libdbexec"
)

var ErrInvalidFunction = errors.New("invalid function data")
var ErrInvalidEventTrigger = errors.New("invalid event trigger data")

type Service interface {
	// Function management
	CreateFunction(ctx context.Context, function *functionstore.Function) error
	GetFunction(ctx context.Context, name string) (*functionstore.Function, error)
	UpdateFunction(ctx context.Context, function *functionstore.Function) error
	DeleteFunction(ctx context.Context, name string) error
	ListFunctions(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*functionstore.Function, error)
	ListAllFunctions(ctx context.Context) ([]*functionstore.Function, error)

	// Event trigger management
	CreateEventTrigger(ctx context.Context, trigger *functionstore.EventTrigger) error
	GetEventTrigger(ctx context.Context, name string) (*functionstore.EventTrigger, error)
	UpdateEventTrigger(ctx context.Context, trigger *functionstore.EventTrigger) error
	DeleteEventTrigger(ctx context.Context, name string) error
	ListEventTriggers(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*functionstore.EventTrigger, error)
	ListAllEventTriggers(ctx context.Context) ([]*functionstore.EventTrigger, error)
	ListEventTriggersByEventType(ctx context.Context, eventType string) ([]*functionstore.EventTrigger, error)
	ListEventTriggersByFunction(ctx context.Context, functionName string) ([]*functionstore.EventTrigger, error)
}

type service struct {
	dbInstance libdb.DBManager
}

func New(db libdb.DBManager) Service {
	return &service{dbInstance: db}
}

func (s *service) CreateFunction(ctx context.Context, function *functionstore.Function) error {
	tx := s.dbInstance.WithoutTransaction()
	if err := validateFunction(function); err != nil {
		return err
	}
	storeInstance := functionstore.New(tx)
	count, err := storeInstance.EstimateFunctionCount(ctx)
	if err != nil {
		return err
	}
	if err := storeInstance.EnforceMaxRowCount(ctx, count); err != nil {
		err := fmt.Errorf("too many rows in the system: %w", err)
		fmt.Printf("SERVER ERROR: creation blocked: limit reached current %d %v", count, err)
		return err
	}

	return storeInstance.CreateFunction(ctx, function)
}

func (s *service) GetFunction(ctx context.Context, name string) (*functionstore.Function, error) {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).GetFunction(ctx, name)
}

func (s *service) UpdateFunction(ctx context.Context, function *functionstore.Function) error {
	if err := validateFunction(function); err != nil {
		return err
	}
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).UpdateFunction(ctx, function)
}

func (s *service) DeleteFunction(ctx context.Context, name string) error {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).DeleteFunction(ctx, name)
}

func (s *service) ListFunctions(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*functionstore.Function, error) {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).ListFunctions(ctx, createdAtCursor, limit)
}

func (s *service) ListAllFunctions(ctx context.Context) ([]*functionstore.Function, error) {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).ListAllFunctions(ctx)
}

func (s *service) CreateEventTrigger(ctx context.Context, trigger *functionstore.EventTrigger) error {
	tx := s.dbInstance.WithoutTransaction()
	if err := validateEventTrigger(trigger); err != nil {
		return err
	}
	storeInstance := functionstore.New(tx)
	count, err := storeInstance.EstimateEventTriggerCount(ctx)
	if err != nil {
		return err
	}
	if err := storeInstance.EnforceMaxRowCount(ctx, count); err != nil {
		err := fmt.Errorf("too many rows in the system: %w", err)
		fmt.Printf("SERVER ERROR: creation blocked: limit reached current %d %v", count, err)
		return err
	}

	return storeInstance.CreateEventTrigger(ctx, trigger)
}

func (s *service) GetEventTrigger(ctx context.Context, name string) (*functionstore.EventTrigger, error) {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).GetEventTrigger(ctx, name)
}

func (s *service) UpdateEventTrigger(ctx context.Context, trigger *functionstore.EventTrigger) error {
	if err := validateEventTrigger(trigger); err != nil {
		return err
	}
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).UpdateEventTrigger(ctx, trigger)
}

func (s *service) DeleteEventTrigger(ctx context.Context, name string) error {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).DeleteEventTrigger(ctx, name)
}

func (s *service) ListEventTriggers(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*functionstore.EventTrigger, error) {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).ListEventTriggers(ctx, createdAtCursor, limit)
}

func (s *service) ListAllEventTriggers(ctx context.Context) ([]*functionstore.EventTrigger, error) {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).ListAllEventTriggers(ctx)
}

func (s *service) ListEventTriggersByEventType(ctx context.Context, eventType string) ([]*functionstore.EventTrigger, error) {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).ListEventTriggersByEventType(ctx, eventType)
}

func (s *service) ListEventTriggersByFunction(ctx context.Context, functionName string) ([]*functionstore.EventTrigger, error) {
	tx := s.dbInstance.WithoutTransaction()
	return functionstore.New(tx).ListEventTriggersByFunction(ctx, functionName)
}

func validateFunction(function *functionstore.Function) error {
	if function.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidFunction)
	}
	if function.ScriptType != string(functionstore.GojaTerm) {
		return fmt.Errorf("%w: scriptType must be 'goja'", ErrInvalidFunction)
	}
	if function.Script == "" {
		return fmt.Errorf("%w: script is required", ErrInvalidFunction)
	}
	return nil
}

func validateEventTrigger(trigger *functionstore.EventTrigger) error {
	if trigger.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidEventTrigger)
	}
	if trigger.ListenFor.Type == "" {
		return fmt.Errorf("%w: listenFor.type is required", ErrInvalidEventTrigger)
	}
	if trigger.Type != string(functionstore.FunctionTerm) {
		return fmt.Errorf("%w: type must be 'function'", ErrInvalidEventTrigger)
	}
	if trigger.Function == "" {
		return fmt.Errorf("%w: function is required", ErrInvalidEventTrigger)
	}
	return nil
}
