package fleetservice

// admission.go enforces the fleet-width cap: Dispatch refuses to open a unit
// once maxParallel units are already open. Enforced uniformly on every
// dispatch; there is no separate bypass path.

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/errdefs"
	"github.com/contenox/contenox/internal/kernel/agentinstance"
)

// MaxParallelConfigKey is the config key an operator sets the fleet-width cap
// with (0 = unlimited).
const MaxParallelConfigKey = "fleet-max-parallel"

// DefaultMaxParallel is the fleet-width cap enforced when no WithMaxParallel
// option is given. 0 means unlimited.
const DefaultMaxParallel = 8

// WithMaxParallel sets the fleet-width admission cap: at most limit units open
// at once, checked by Dispatch before it allocates. limit <= 0 means unlimited.
func WithMaxParallel(limit int) Option {
	return func(s *service) {
		if limit < 0 {
			limit = 0
		}
		s.maxParallel = limit
	}
}

// admissionRefusalLead is the stable prefix of every cap refusal, used to
// distinguish it from other dispatch errors.
const admissionRefusalLead = "fleet admission refused"

// admissionRefusal builds the refusal message for a dispatch past the cap,
// naming the current count, the cap, and how to raise it.
func admissionRefusal(open, limit int) string {
	return fmt.Sprintf(
		"%s: %d units are already open, at the fleet-width cap (%s=%d). Wait for a unit to conclude or stop one, or raise the cap: `contenox config set %s <N>` (0 = unlimited).",
		admissionRefusalLead, open, MaxParallelConfigKey, limit, MaxParallelConfigKey)
}

// unitOpen reports whether an instance state holds a slot of fleet width:
// starting and running do; crashed (error/warning) instances do not, so dead
// wreckage cannot permanently block new dispatches.
func unitOpen(st agentinstance.InstanceStatus) bool {
	return st.State == agentinstance.StateStarting || st.State == agentinstance.StateRunning
}

// openUnits counts the units currently holding fleet width, across all agents.
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

// admitUnit checks the fleet-width cap: nil admits, non-nil refuses. Callers
// must hold s.admission across this check and the allocation that follows
// (StartResolved), so two concurrent dispatches cannot both read cap-1 and both
// allocate. Fails closed if the open count cannot be read.
func (s *service) admitUnit(ctx context.Context) error {
	if s.maxParallel <= 0 {
		return nil // 0 = unlimited.
	}
	open, err := s.openUnits(ctx)
	if err != nil {
		return err
	}
	if open < s.maxParallel {
		return nil
	}
	recordCapRefusal()
	// 409: reflects fleet state, not request shape; retrying after a unit
	// concludes is legitimate.
	return errdefs.Conflict(admissionRefusal(open, s.maxParallel))
}
