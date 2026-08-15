package missiontools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missionservice"
)

// ToolNameStartMission is the supervisor's other half: mission_list reads the
// subagents a session already has, this one starts a new one. Together they are
// what lets a session plan work and then run each step as a subagent, without a
// slash command in the loop.
const ToolNameStartMission = "mission_start"

// SubagentSpec is one dispatch request, in this package's own vocabulary.
// fleetservice imports missiontools for its tool names, so the dependency
// cannot run the other way: the host wires an adapter over fleetservice.Dispatch.
type SubagentSpec struct {
	AgentName       string
	Intent          string
	HITLPolicyName  string
	ParentSessionID string
}

// SubagentHandle is what a dispatch returns.
type SubagentHandle struct {
	MissionID string
}

// Spawner starts one subagent. The narrow slice of the fleet this package needs.
type Spawner interface {
	Spawn(ctx context.Context, spec SubagentSpec) (SubagentHandle, error)
}

// SubagentWatcher reads one subagent to rest: its record, and what it reported.
// missionservice.Service satisfies it.
type SubagentWatcher interface {
	Get(ctx context.Context, id string) (*missionservice.Mission, error)
	ListReports(ctx context.Context, missionID string, limit int) ([]*missionservice.Report, error)
}

// SubagentDefaults resolves the agent and envelope a start with neither named
// runs under. Read per call rather than captured at construction, so a config
// change lands without a restart.
type SubagentDefaults func(ctx context.Context) (agentName, hitlPolicyName string)

const (
	// defaultSubagentTimeout bounds one mission_start call. A subagent that has
	// not reached rest by then keeps running and keeps its record; only the
	// waiting stops, and the caller is told so plainly.
	defaultSubagentTimeout = 15 * time.Minute
	// subagentPollInterval is how often the wait re-reads the record. The
	// mission store is local, so this is cheap.
	subagentPollInterval = 2 * time.Second
	// startedReportLimit bounds the reports handed back on completion.
	startedReportLimit = 20
)

// WithSpawner wires the fleet a supervising session starts subagents through;
// unset, mission_start is not offered at all.
func WithSpawner(spawner Spawner, watcher SubagentWatcher, defaults SubagentDefaults) Option {
	return func(p *provider) {
		p.spawner = spawner
		p.watcher = watcher
		p.subagentDefaults = defaults
	}
}

// WithSubagentTimeout overrides how long one mission_start waits for rest.
func WithSubagentTimeout(d time.Duration) Option {
	return func(p *provider) {
		if d > 0 {
			p.subagentTimeout = d
		}
	}
}

// canSpawn reports whether mission_start can run: a fleet to dispatch through
// and a store to watch. Never advertise what cannot work.
func (p *provider) canSpawn() bool {
	return p.spawner != nil && p.watcher != nil
}

func startMissionToolSchema() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name: ToolNameStartMission,
			Description: "Run one unit of work as a SUBAGENT and wait for it to finish. " +
				"The subagent is a fresh agent with its own context: it does not see this conversation, so `intent` must be a complete, self-contained instruction — what to do, where, and what counts as done. " +
				"Use it to execute a step of your plan, or any piece of work worth isolating. " +
				"It returns the subagent's final status and everything it reported. " +
				"This BLOCKS until the subagent reaches a terminal state, so run one step at a time and read its result before starting the next.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"intent": map[string]any{
						"type":        "string",
						"description": "The complete, self-contained instruction for the subagent. It has none of your context — state the goal, the paths, and what a finished result looks like.",
					},
					"agent": map[string]any{
						"type":        "string",
						"description": "Which declared agent to run it as. Omit to use the configured default subagent.",
					},
					"policy": map[string]any{
						"type":        "string",
						"description": "The envelope (HITL policy) bounding it. Omit to use the configured default.",
					},
				},
				"required": []string{"intent"},
			},
		},
	}
}

func (p *provider) execStartMission(ctx context.Context, parentSessionID string, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if !p.canSpawn() {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: starting a subagent is not wired in this process")
	}
	intent := strings.TrimSpace(argString(input, call, "intent"))
	if intent == "" {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %s requires an 'intent' — the self-contained instruction the subagent runs", ToolNameStartMission)
	}
	// An intent is one line by contract (missionservice.validate); collapsing it
	// here turns a model's multi-line paste into a valid intent instead of a
	// validation error it cannot see the reason for.
	intent = strings.Join(strings.Fields(intent), " ")

	agentName := strings.TrimSpace(argString(input, call, "agent"))
	policyName := strings.TrimSpace(argString(input, call, "policy"))
	if (agentName == "" || policyName == "") && p.subagentDefaults != nil {
		defaultAgent, defaultPolicy := p.subagentDefaults(ctx)
		if agentName == "" {
			agentName = strings.TrimSpace(defaultAgent)
		}
		if policyName == "" {
			policyName = strings.TrimSpace(defaultPolicy)
		}
	}
	if agentName == "" {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %s needs an agent: name one, or set a default with `contenox config set default-mission-agent <name>`", ToolNameStartMission)
	}
	if policyName == "" {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %s needs an envelope: name one, or set a default with `contenox config set default-mission-policy <name>`", ToolNameStartMission)
	}

	handle, err := p.spawner.Spawn(ctx, SubagentSpec{
		AgentName:       agentName,
		Intent:          intent,
		HITLPolicyName:  policyName,
		ParentSessionID: parentSessionID,
	})
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: start subagent: %w", err)
	}

	m, waitErr := p.waitForRest(ctx, handle.MissionID)
	out := map[string]any{
		"missionId": handle.MissionID,
		"agent":     agentName,
		"intent":    intent,
	}
	if m != nil {
		out["status"] = string(m.Status)
		if m.StatusReason != "" {
			out["statusReason"] = m.StatusReason
		}
	}
	if waitErr != nil {
		// Not an error result: the subagent is real and still running, and the
		// caller can read it later. Saying so beats failing the tool call.
		out["status"] = "running"
		out["note"] = fmt.Sprintf("still running after %s; it keeps its record — read it later with %s", p.timeout(), ToolNameListMissions)
	}
	out["reports"] = p.reportSummaries(ctx, handle.MissionID)

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: encode subagent outcome: %w", err)
	}
	return string(raw), taskengine.DataTypeString, nil
}

func (p *provider) timeout() time.Duration {
	if p.subagentTimeout > 0 {
		return p.subagentTimeout
	}
	return defaultSubagentTimeout
}

// waitForRest polls one subagent's record until it is terminal, the deadline
// passes, or the caller's turn is cancelled.
func (p *provider) waitForRest(ctx context.Context, missionID string) (*missionservice.Mission, error) {
	deadline := time.Now().Add(p.timeout())
	ticker := time.NewTicker(subagentPollInterval)
	defer ticker.Stop()
	for {
		m, err := p.watcher.Get(ctx, missionID)
		if err == nil && m != nil && missionservice.IsTerminalStatus(m.Status) {
			return m, nil
		}
		if time.Now().After(deadline) {
			return m, fmt.Errorf("subagent %s did not finish within %s", missionID, p.timeout())
		}
		select {
		case <-ctx.Done():
			return m, ctx.Err()
		case <-ticker.C:
		}
	}
}

// reportSummaries renders what the subagent reported, oldest first, so the
// caller reads it in the order the subagent found it.
func (p *provider) reportSummaries(ctx context.Context, missionID string) []map[string]any {
	reports, err := p.watcher.ListReports(ctx, missionID, startedReportLimit)
	if err != nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(reports))
	for i := len(reports) - 1; i >= 0; i-- {
		r := reports[i]
		entry := map[string]any{"kind": string(r.Kind), "summary": r.Summary}
		if r.Detail != "" {
			entry["detail"] = r.Detail
		}
		if len(r.Refs) > 0 {
			entry["refs"] = r.Refs
		}
		if r.Handover != nil {
			entry["handover"] = r.Handover
		}
		out = append(out, entry)
	}
	return out
}
