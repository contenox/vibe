package fleetservice

// counters.go is the fleet's minimal telemetry ledger: three process-lifetime
// tallies of what the envelope-enforcement seams actually DID — units admitted
// (dispatches), units refused at the width cap (capRefusals), and result reports
// the conclusion gate downgraded (verificationDowngrades). One optional line for
// doctor / the mission panel to render, per the pando mining report's pairing
// rule ("pair with nil-safe atomic counters … surfaced as one optional line"):
// an enforced bound the operator cannot SEE working is indistinguishable from an
// unenforced one, which is exactly the anti-pattern the cap exists to avoid.
//
// They are PACKAGE-LEVEL atomics, deliberately — not fields on the service:
//
//   - Nil-safe by construction: there is no receiver to be nil, so any path may
//     bump or read them — a panic-recovery handler included — without a guard.
//   - Process-lifetime by construction: they survive any service value and count
//     across every fleetservice instance in the process (the in-process editor
//     builds exactly one; if a process ever built two, one fleet-wide ledger is
//     still the honest total).
//   - Reachable without wiring: doctor or a panel calls fleetservice.Counters()
//     with no handle on the service at all.
//
// They are NOT durable and reset on restart — telemetry, not record. Anything
// that must survive a restart belongs on the mission store, not here.

import "sync/atomic"

// fleetCounters is the ledger itself. Zero value ready; atomics only, so every
// bump is safe from any goroutine with no lock.
var fleetCounters struct {
	dispatches             atomic.Uint64
	capRefusals            atomic.Uint64
	verificationDowngrades atomic.Uint64
}

// CountersSnapshot is one consistent-enough read of the fleet counters (each
// field is read atomically; the trio is not a transaction, which is fine for a
// telemetry line). JSON tags so a doctor/panel surface can render it verbatim.
type CountersSnapshot struct {
	// Dispatches counts units this process ADMITTED — Dispatch calls that passed
	// the admission gate and allocated a unit end to end.
	Dispatches uint64 `json:"dispatches"`
	// CapRefusals counts dispatches REFUSED at the fleet-width admission cap
	// (see admission.go). A nonzero value is the cap visibly working.
	CapRefusals uint64 `json:"capRefusals"`
	// VerificationDowngrades counts result reports the conclusion verification
	// gate downgraded to progress because a claimed artifact was positively
	// missing (see missiontools' verification gate, which reports here through
	// RecordVerificationDowngrade).
	VerificationDowngrades uint64 `json:"verificationDowngrades"`
}

// Counters returns the current fleet counter snapshot. This is the one surface
// a doctor line or the mission panel reads; it requires no service handle and
// never fails.
func Counters() CountersSnapshot {
	return CountersSnapshot{
		Dispatches:             fleetCounters.dispatches.Load(),
		CapRefusals:            fleetCounters.capRefusals.Load(),
		VerificationDowngrades: fleetCounters.verificationDowngrades.Load(),
	}
}

// RecordVerificationDowngrade bumps the verification-downgrade tally. It is
// EXPORTED because the gate that observes downgrades lives in missiontools,
// which fleetservice imports (for the tool-name constants) — so missiontools
// cannot import this package back. The composition point closes the loop
// instead: missiontools.WithDowngradeRecorder(fleetservice.RecordVerificationDowngrade).
// Safe from any goroutine, at any time, wired or not.
func RecordVerificationDowngrade() { fleetCounters.verificationDowngrades.Add(1) }

func recordDispatch() { fleetCounters.dispatches.Add(1) }

func recordCapRefusal() { fleetCounters.capRefusals.Add(1) }
