package fleetservice

import (
	"fmt"
	"sync"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/libacp"
)

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

func turnBudgetExceeded(nextTurn int, b hitlservice.ComputeBounds) bool {
	return b.MaxTurns > 0 && nextTurn > b.MaxTurns
}

func toolCallBudgetExceeded(count int, b hitlservice.ComputeBounds) bool {
	return b.MaxToolCalls > 0 && count > b.MaxToolCalls
}

func tokenBudgetExceeded(used int, b hitlservice.ComputeBounds) bool {
	return b.MaxTokens > 0 && used > b.MaxTokens
}

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

const defaultMaxTrackedMissions = 4096

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
