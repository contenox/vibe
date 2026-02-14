package stateservice

import (
	"context"

	"github.com/contenox/vibe/internal/runtimestate"
	"github.com/contenox/vibe/statetype"
)

type Service interface {
	Get(ctx context.Context) ([]statetype.BackendRuntimeState, error)
}

type service struct {
	state *runtimestate.State
}

// Get implements Service.
func (s *service) Get(ctx context.Context) ([]statetype.BackendRuntimeState, error) {
	m := s.state.Get(ctx)
	l := make([]statetype.BackendRuntimeState, 0, len(m))
	for _, e := range m {
		l = append(l, e)
	}
	return l, nil
}

func New(state *runtimestate.State) Service {
	return &service{
		state: state,
	}
}
