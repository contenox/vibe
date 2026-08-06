package fleetservice

// compute.go enforces the envelope's compute bounds (hitlservice.ComputeBounds:
// turns, gated tool dispatches, tokens) at two host-side seams: the drive loop
// (maxTurns, maxTokens) and the unattended answerer (maxToolCalls). A mission
// that crosses a bound is finished stuck via missionservice.Finish, not
// silently no-opped. Absent bounds (every field zero) are unbounded.

import (
	"fmt"
	"sync"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/libacp"
)

// computeBoundLead is the stable prefix of every compute-exhaustion reason a
// mission is finished stuck with, distinguishing it from any other stuck.
const computeBoundLead = "compute bound exhausted"

func turnsExhaustedReason(b hitlservice.ComputeBounds) string {
	return fmt.Sprintf("%s: maxTurns=%d — the mission spent its turn budget without reaching its operator.", computeBoundLead, b.MaxTurns)
}

func toolCallsExhaustedReason(b hitlservice.ComputeBounds) string {
	return fmt.Sprintf("%s: maxToolCalls=%d — the mission reached its envelope-gated action budget; this call and any after it are refused.", computeBoundLead, b.MaxToolCalls)
}

func tokensExhaustedReason(b hitlservice.ComputeBounds, used int) string {
	return fmt.Sprintf("%s: maxTokens=%d (reported usage %d) — the mission spent its token budget.", computeBoundLead, b.MaxTokens, used)
}

// turnBudgetExceeded reports whether starting the nextTurn'th prompt turn
// (1-based) would exceed maxTurns. Unbounded (0) never exceeds.
func turnBudgetExceeded(nextTurn int, b hitlservice.ComputeBounds) bool {
	return b.MaxTurns > 0 && nextTurn > b.MaxTurns
}

// toolCallBudgetExceeded reports whether count gated dispatches crosses
// maxToolCalls: with N set, the first N pass and the (N+1)th is refused.
func toolCallBudgetExceeded(count int, b hitlservice.ComputeBounds) bool {
	return b.MaxToolCalls > 0 && count > b.MaxToolCalls
}

// tokenBudgetExceeded reports whether reported token usage crosses maxTokens.
func tokenBudgetExceeded(used int, b hitlservice.ComputeBounds) bool {
	return b.MaxTokens > 0 && used > b.MaxTokens
}

// journalTokenUsage extracts the max Used across every usage_update in the
// session journal (a cumulative total, so max is the latest figure). present
// is false with no usage_update at all, leaving maxTokens inert rather than
// enforced against a phantom zero.
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

// defaultMaxTrackedMissions bounds the unattended answerer's per-mission
// gated-call tally so a long-lived serve cannot leak a counter per mission
// forever.
const defaultMaxTrackedMissions = 4096

// missionCallCounter is the unattended answerer's in-memory per-mission tally
// of envelope-gated tool dispatches, checked against maxToolCalls. Bounded
// (see defaultMaxTrackedMissions) with FIFO eviction of the oldest tracked
// mission; an evicted mission that raises another gated call simply restarts
// its count.
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
