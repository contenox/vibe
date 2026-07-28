package fleetservice

// counters.go is the fleet's telemetry ledger: process-lifetime tallies of
// units admitted, units refused at the width cap, and result reports the
// conclusion gate downgraded. Package-level atomics (nil-safe, reachable
// without a service handle), not durable, and reset on restart.

import "sync/atomic"

// fleetCounters is the ledger itself. Zero value ready; atomics only, so every
// bump is safe from any goroutine with no lock.
var fleetCounters struct {
	dispatches             atomic.Uint64
	capRefusals            atomic.Uint64
	verificationDowngrades atomic.Uint64
}

// CountersSnapshot is one consistent-enough read of the fleet counters: each
// field is read atomically, but the trio is not a transaction.
type CountersSnapshot struct {
	// Dispatches counts units this process admitted end to end.
	Dispatches uint64 `json:"dispatches"`
	// CapRefusals counts dispatches refused at the fleet-width admission cap
	// (see admission.go).
	CapRefusals uint64 `json:"capRefusals"`
	// VerificationDowngrades counts result reports the conclusion verification
	// gate downgraded to progress because a claimed artifact was positively
	// missing.
	VerificationDowngrades uint64 `json:"verificationDowngrades"`
}

// Counters returns the current fleet counter snapshot. Requires no service
// handle and never fails.
func Counters() CountersSnapshot {
	return CountersSnapshot{
		Dispatches:             fleetCounters.dispatches.Load(),
		CapRefusals:            fleetCounters.capRefusals.Load(),
		VerificationDowngrades: fleetCounters.verificationDowngrades.Load(),
	}
}

// RecordVerificationDowngrade bumps the verification-downgrade tally. Exported
// because the gate that observes downgrades lives in missiontools, which
// fleetservice imports, so missiontools cannot import this package back.
func RecordVerificationDowngrade() { fleetCounters.verificationDowngrades.Add(1) }

func recordDispatch() { fleetCounters.dispatches.Add(1) }

func recordCapRefusal() { fleetCounters.capRefusals.Add(1) }
