package fleetservice

// admission.go is the fleet's WIDTH boundary — the sibling of compute.go's
// per-mission depth bounds. ComputeBounds caps what ONE mission may spend;
// nothing bounded how many units could be open at once, so a mission loop (or an
// enthusiastic operator) could widen the fleet without limit. This file is the
// admission gate that closes that hole: before Dispatch allocates a unit it
// counts the units already open and refuses past a ceiling, with a refusal that
// teaches — it names the cap, its value, and both remedies — instead of merely
// failing (the same "gates that teach" register as compute.go's exhaustion
// reasons).
//
// The pando lesson this file exists to honor: NEVER expose the knob before it
// is enforced. The cap ships enforced at its default in the same change that
// names its config key — a fleetservice built with no option at all already
// refuses the (DefaultMaxParallel+1)th unit.
//
// Bypass policy: there is nothing to carve out today. Dispatch is this
// service's ONLY door into allocation — fleetservice has no separate operator
// relaunch/retry verb — so the cap is enforced uniformly on every dispatch,
// automatic or operator-fired. If a distinct operator relaunch path ever
// appears, the doctrine is already decided (pando mining, item 2): explicit
// operator retry MAY bypass, automatic dispatch NEVER may.

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/errdefs"
	"github.com/contenox/beam/internal/kernel/agentinstance"
)

// MaxParallelConfigKey is the clikv config key an operator sets the fleet-width
// cap with (`contenox config set fleet-max-parallel <N>`; 0 = unlimited). It is
// declared HERE, next to the enforcement, and exported so the CLI's config
// surface and the serve wiring reference this constant rather than re-typing the
// string — the key and the gate cannot drift apart.
const MaxParallelConfigKey = "fleet-max-parallel"

// DefaultMaxParallel is the fleet-width ceiling a fleetservice enforces when no
// WithMaxParallel option is given — the enforced-from-birth default (8 units:
// generous for one workstation's missions, small enough that a runaway mission
// loop is stopped before it matters). 0 means UNLIMITED, which is therefore a
// choice an operator must make explicitly, never a default they fell into.
const DefaultMaxParallel = 8

// WithMaxParallel wires the fleet-width admission cap: at most limit units open
// at once, checked by Dispatch before it allocates. limit <= 0 means UNLIMITED
// (the documented meaning of fleet-max-parallel=0). Without this option the
// service enforces DefaultMaxParallel — the knob is born enforced; wiring only
// ever CHANGES the value.
func WithMaxParallel(limit int) Option {
	return func(s *service) {
		if limit < 0 {
			limit = 0
		}
		s.maxParallel = limit
	}
}

// admissionRefusalLead is the stable, greppable lead of every cap refusal, the
// exact analogue of compute.go's computeBoundLead: an operator (or a test) keys
// on it to tell a width refusal apart from any other dispatch error.
const admissionRefusalLead = "fleet admission refused"

// admissionRefusal builds the teaching refusal for a dispatch past the cap. It
// names the count it saw, the cap and its config key and value, and BOTH
// remedies — wait for (or stop) a unit, or raise the cap — so the refusal is a
// lesson in the envelope, not a dead end.
func admissionRefusal(open, limit int) string {
	return fmt.Sprintf(
		"%s: %d units are already open, at the fleet-width cap (%s=%d). Wait for a unit to conclude or stop one, or raise the cap: `contenox config set %s <N>` (0 = unlimited).",
		admissionRefusalLead, open, MaxParallelConfigKey, limit, MaxParallelConfigKey)
}

// unitOpen is the liveness predicate the admission count rests on: which
// kernel-reported instance states hold a slot of fleet width. Starting and
// running do — a unit mid-spawn is spend already committed. The wreckage states
// do NOT: an error/warning instance is a crashed subprocess awaiting the
// watchdog or an operator, not a unit doing work, and counting it would let
// dead wreckage permanently block new dispatches (the cap must bound spend, not
// punish crashes). Stopped instances never appear at all — the kernel removes
// them from its registry on Stop — which is what makes this a count of RUNNING
// units rather than of finished rows.
func unitOpen(st agentinstance.InstanceStatus) bool {
	return st.State == agentinstance.StateStarting || st.State == agentinstance.StateRunning
}

// openUnits counts the units currently holding fleet width, straight off the
// kernel's own board (agentinstance.Manager.List) — the same join the fleet
// surface renders, so the number the gate enforces is the number an operator
// sees. The count is GLOBAL across agents: the cap bounds the fleet, not one
// agent's share (a per-agent ceiling is the blueprint's noted "optionally per
// agent" extension, not this slice).
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

// admitUnit is the gate itself: nil admits, an error refuses with the teaching
// message. The caller MUST hold s.admission across this check AND the
// allocation that follows (StartResolved), so two concurrent dispatches cannot
// both read cap-1 and both allocate — the count-then-allocate window is closed
// by the lock, not by hope.
//
// Fail-CLOSED on a count that cannot be read: a gate that cannot see the board
// must not wave units through, and the kernel's List failing is a real defect
// worth surfacing, not a hiccup to dispatch past.
func (s *service) admitUnit(ctx context.Context) error {
	if s.maxParallel <= 0 {
		return nil // 0 = unlimited, the documented opt-out.
	}
	open, err := s.openUnits(ctx)
	if err != nil {
		return err
	}
	if open < s.maxParallel {
		return nil
	}
	recordCapRefusal()
	// A 409: the refusal is about the fleet's current STATE (too many open
	// units), not about this request's shape — retrying after a unit concludes
	// is legitimate, which is exactly what Conflict signals.
	return errdefs.Conflict(admissionRefusal(open, s.maxParallel))
}
