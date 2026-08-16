package fleetservice

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/errdefs"
	"github.com/contenox/contenox/internal/kernel/agentinstance"
)

// MaxParallelConfigKey is the config key an operator sets the fleet-width cap
// with (0 = unlimited).
const MaxParallelConfigKey = "fleet-max-parallel"

// DefaultMaxParallel is the fleet-width cap enforced when no WithMaxParallel option is given (0 means unlimited).
const DefaultMaxParallel = 8

// WithMaxParallel sets the fleet-width admission cap checked by Dispatch before it allocates; limit <= 0 means unlimited.
func WithMaxParallel(limit int) Option {
	return func(s *service) {
		if limit < 0 {
			limit = 0
		}
		s.maxParallel = limit
	}
}

const admissionRefusalLead = "fleet admission refused"

func admissionRefusal(open, limit int) string {
	return fmt.Sprintf(
		"%s: %d units are already open, at the fleet-width cap (%s=%d). Wait for a unit to conclude or stop one, or raise the cap: `contenox config set %s <N>` (0 = unlimited).",
		admissionRefusalLead, open, MaxParallelConfigKey, limit, MaxParallelConfigKey)
}

func unitOpen(st agentinstance.InstanceStatus) bool {
	return st.State == agentinstance.StateStarting || st.State == agentinstance.StateRunning
}

func (s *service) openUnits(ctx context.Context) (int, error) {
	entries, err := s.instances.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("fleetservice: count open units for admission: %w", err)
	}
	open := 0
	for _, e := range entries {
		for _, st := range e.Instances {
			if unitOpen(st) {
				open++
			}
		}
	}
	return open, nil
}

func (s *service) admitUnit(ctx context.Context) error {
	if s.maxParallel <= 0 {
		return nil
	}
	open, err := s.openUnits(ctx)
	if err != nil {
		return err
	}
	if open < s.maxParallel {
		return nil
	}
	recordCapRefusal()
	// Reflects fleet state, not request shape: retrying later is legitimate.
	return errdefs.Conflict(admissionRefusal(open, s.maxParallel))
}
