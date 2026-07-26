package fleetservice

// compute.go is the fleet's half of the envelope's COMPUTE boundary — the
// constitutional widening of the mission envelope from "what a unit may DO" (the
// HITL action rules) to "how much a unit may SPEND" (its turns, its gated tool
// dispatches, its tokens). The bounds themselves live on the envelope
// (hitlservice.ComputeBounds); this file is the enforcement that makes them real
// at the two deterministic HOST-side seams a dispatched mission passes through:
//
//   - the DRIVE LOOP (driveUnattendedMission): counts the prompt turns it itself
//     issues, so maxTurns is a local, exact count — and reads the unit's reported
//     token usage from the session journal between turns, so maxTokens is enforced
//     best-effort from what the downstream actually reported.
//   - the UNATTENDED ANSWERER (unattended.go): every envelope-gated tool dispatch a
//     mission raises passes through it for a verdict, which makes it the one host
//     seam that can COUNT gated dispatches AND refuse the call that crosses the
//     bound with a teaching outcome (maxToolCalls).
//
// These are GATES, deliberately — the legitimate kind the tool-hardening doctrine
// names: envelope-declared, operator-authored, deterministic at the boundary. When
// a mission crosses a bound the runtime does not silently no-op it: it finishes the
// mission stuck through the real terminal machinery (missionservice.Finish), so the
// board, the inbox, and a `--wait` all tell the truth for free. Absent bounds
// (every field zero) are unbounded — today's behavior — so this file only ever
// RESTRICTS.

import (
	"fmt"
	"sync"

	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/libacp"
)

// computeBoundLead is the stable, greppable lead of every compute-exhaustion
// reason a mission is finished stuck with. An operator (or a test) keys on it to
// tell a compute-bound stop apart from any other stuck, and each specific reason
// names WHICH bound and its value so the terminal record teaches, not just refuses.
const computeBoundLead = "compute bound exhausted"

func turnsExhaustedReason(limit int) string {
	return fmt.Sprintf("%s: maxTurns=%d — the mission spent its turn budget without reaching its operator.", computeBoundLead, limit)
}

func toolCallsExhaustedReason(limit int) string {
	return fmt.Sprintf("%s: maxToolCalls=%d — the mission reached its envelope-gated action budget; this call and any after it are refused.", computeBoundLead, limit)
}

func tokensExhaustedReason(limit, used int) string {
	return fmt.Sprintf("%s: maxTokens=%d (reported usage %d) — the mission spent its token budget.", computeBoundLead, limit, used)
}

// turnBudgetExceeded reports whether STARTING the nextTurn'th prompt turn (1-based)
// would exceed the mission's maxTurns bound. Unbounded (MaxTurns == 0) never
// exceeds, so the default is exactly today's behavior. It is the predicate the
// drive loop checks before issuing a turn — a check BEFORE, not after, so a turn
// past the budget is never spent in the first place.
func turnBudgetExceeded(nextTurn int, b hitlservice.ComputeBounds) bool {
	return b.MaxTurns > 0 && nextTurn > b.MaxTurns
}

// toolCallBudgetExceeded reports whether count gated tool dispatches (the total
// AFTER this call was counted) crosses the mission's maxToolCalls bound. With
// MaxToolCalls == N the first N calls pass and the (N+1)th is the one refused;
// unbounded (0) never exceeds.
func toolCallBudgetExceeded(count int, b hitlservice.ComputeBounds) bool {
	return b.MaxToolCalls > 0 && count > b.MaxToolCalls
}

// tokenBudgetExceeded reports whether the unit's reported token usage crosses the
// mission's maxTokens bound. Unbounded (0) never exceeds.
func tokenBudgetExceeded(used int, b hitlservice.ComputeBounds) bool {
	return b.MaxTokens > 0 && used > b.MaxTokens
}

// journalTokenUsage extracts the mission's reported token usage from its session
// journal: the MAX Used across every usage_update the downstream emitted. ACP
// usage_update carries a cumulative session total (see acpsvc's TaskEventTokenUsage
// path), so the max is the latest honest figure regardless of journal ordering.
//
// present is false when the journal carries no usage_update at all — the "this unit
// reports no usage" case (a provider that emits none, a deterministic chain that
// resolves no model). That leaves maxTokens INERT for such a unit rather than
// enforcing it against a phantom zero: the honest behavior for a best-effort
// signal, and exactly why maxTokens is documented as best-effort on the envelope.
func journalTokenUsage(notes []libacp.SessionNotification) (used int, present bool) {
	for _, n := range notes {
		if n.Update.SessionUpdate != libacp.SessionUpdateUsageUpdate {
			continue
		}
		present = true
		if n.Update.Used > used {
			used = n.Update.Used
		}
	}
	return used, present
}

// defaultMaxTrackedMissions bounds the unattended answerer's per-mission gated-call
// tally so a long-lived serve cannot leak a counter per mission forever. It is
// generous — missions are dispatches on one workstation (the same reasoning
// missionservice.GetByInstance's scan rests on) — and eviction only ever drops a
// stale mission's count (one that has since finished), never a live one mid-run.
const defaultMaxTrackedMissions = 4096

// missionCallCounter is the unattended answerer's per-mission tally of
// envelope-gated tool dispatches — the count maxToolCalls is checked against. It is
// in-memory by necessity: there is no durable per-mission tool-call count, and the
// tally only needs to outlive a single mission's run inside one process. It is
// bounded (see defaultMaxTrackedMissions) with FIFO eviction of the oldest tracked
// mission, so memory stays bounded on a 24/7 serve; an evicted mission that somehow
// raises another gated call simply restarts its count, a benign degradation under
// the kind of load (thousands of concurrent missions) that would trigger it.
type missionCallCounter struct {
	mu     sync.Mutex
	counts map[string]int
	order  []string
	max    int
}

func newMissionCallCounter(max int) *missionCallCounter {
	if max <= 0 {
		max = defaultMaxTrackedMissions
	}
	return &missionCallCounter{counts: map[string]int{}, max: max}
}

// increment bumps missionID's gated-call count and returns the NEW total.
func (c *missionCallCounter) increment(missionID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, seen := c.counts[missionID]
	if !seen {
		if len(c.counts) >= c.max && len(c.order) > 0 {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.counts, oldest)
		}
		c.order = append(c.order, missionID)
	}
	n++
	c.counts[missionID] = n
	return n
}
