package chatsessionmodes

import (
	"context"
	"time"

	"github.com/contenox/contenox/runtime/taskengine"
)

// InjectInput is passed to each Injector for one turn.
type InjectInput struct {
	SessionID string
	Now       time.Time
	Turn      TurnInput
}

// Injector adds zero or more system messages before the new user message.
type Injector interface {
	Inject(ctx context.Context, in InjectInput) ([]taskengine.Message, error)
}
