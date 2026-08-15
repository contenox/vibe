package missiontools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missionservice"
)

// The supervisor tools, unlocked for a session that HAS subagents rather than IS one.
const (
	// ToolNameListMissions lists the subagents this session fired.
	ToolNameListMissions = "mission_list"
	// ToolNameAnswer answers a question raised by one of this session's subagents.
	ToolNameAnswer = "mission_answer"
)

// SupervisorStore is what a firing session may read: its own missions only,
// looked up by parent session id rather than a general list a caller could widen.
type SupervisorStore interface {
	// MissionsFiredBy returns the missions whose ParentSessionID is
	// parentSessionID, newest first.
	MissionsFiredBy(ctx context.Context, parentSessionID string, limit int) ([]*missionservice.Mission, error)
	// ListReports returns a mission's reports, newest first.
	ListReports(ctx context.Context, missionID string, limit int) ([]*missionservice.Report, error)
}

// PendingAsk is one unanswered question from a unit: the question, and the
// handle to answer it with.
type PendingAsk struct {
	AskID     string `json:"askId"`
	MissionID string `json:"missionId"`
	Question  string `json:"question"`
	Detail    string `json:"detail,omitempty"`
	AskedAt   string `json:"askedAt,omitempty"`
}

// AttentionResolver is the answering half of the supervisor surface: find what a subagent is waiting on, and answer it.
type AttentionResolver interface {
	// PendingAsks returns the unanswered questions for missionID.
	PendingAsks(ctx context.Context, missionID string) ([]PendingAsk, error)
	// AnswerAsAgent resolves askID with text as an agent answer; an *AnswerRefusedError means the envelope declined it.
	AnswerAsAgent(ctx context.Context, askID, text string) error
}

// AnswerRefusedError is the mission envelope declining an agent answer. Reason is discarded here, so the resolver must state it on its own trace.
type AnswerRefusedError struct{ Reason string }

func (e *AnswerRefusedError) Error() string { return e.Reason }

// WithParentSessionID marks ctx as belonging to a session that may supervise its own missions — the mirror of WithMissionID: that says you ARE a mission, this says you HAVE missions.
func WithParentSessionID(ctx context.Context, contenoxSessionID string) context.Context {
	if strings.TrimSpace(contenoxSessionID) == "" {
		return ctx
	}
	return context.WithValue(ctx, parentSessionCtxKey{}, contenoxSessionID)
}

type parentSessionCtxKey struct{}

// ParentSessionIDFromContext returns the supervising session id bound by
// WithParentSessionID, or "" when the caller supervises nothing.
func ParentSessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(parentSessionCtxKey{}).(string)
	return id
}

func (p *provider) supervisorTools() []taskengine.Tool {
	tools := []taskengine.Tool{listMissionsToolSchema(p.resolver != nil)}
	if p.canSpawn() {
		tools = append(tools, startMissionToolSchema())
	}
	if p.resolver != nil {
		tools = append(tools, answerToolSchema())
	}
	return tools
}

func listMissionsToolSchema(canAnswer bool) taskengine.Tool {
	description := "List the subagents YOU fired from this session — their intent, status, last heartbeat, and latest reports. " +
		"Call it when you need to know what your subagents are doing, or before answering the user about them."
	if canAnswer {
		description = "List the subagents YOU fired from this session — their intent, status, last heartbeat, latest reports, and any QUESTION a subagent is currently waiting on you to answer. " +
			"Call it when you need to know what your subagents are doing, before answering the user about them, or to get the `askId` you need for " + ToolNameAnswer + "."
	}
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        ToolNameListMissions,
			Description: description,
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func answerToolSchema() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name: ToolNameAnswer,
			Description: "Answer a QUESTION one of your own subagents is blocked on. The subagent is parked waiting for this: your text becomes the result of the tool call it asked with, and it continues immediately. " +
				"Get `askId` from " + ToolNameListMissions + " (or from the question you were just shown). Answer only what you actually know — if the answer needs the user, ask THEM first and relay it; a confident wrong answer sends the subagent down the wrong path.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"askId": map[string]any{
						"type":        "string",
						"description": "The id of the question to answer.",
					},
					"answer": map[string]any{
						"type":        "string",
						"description": "The answer, in your own words — what the subagent needs to proceed.",
					},
				},
				"required": []string{"askId", "answer"},
			},
		},
	}
}

func (p *provider) execListMissions(ctx context.Context, parentSessionID string) (any, taskengine.DataType, error) {
	if p.supervisor == nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: mission supervision is not wired in this process")
	}
	missions, err := p.supervisor.MissionsFiredBy(ctx, parentSessionID, supervisorMissionLimit)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: list fired missions: %w", err)
	}
	out := make([]map[string]any, 0, len(missions))
	for _, m := range missions {
		entry := map[string]any{
			"missionId": m.ID,
			"agentName": m.AgentName,
			"intent":    m.Intent,
			"status":    string(m.Status),
		}
		if m.StatusReason != "" {
			entry["statusReason"] = m.StatusReason
		}
		if m.LastHeartbeat != nil {
			entry["lastHeartbeat"] = m.LastHeartbeat.UTC().Format(time.RFC3339)
		}
		if reports, err := p.supervisor.ListReports(ctx, m.ID, supervisorReportLimit); err == nil && len(reports) > 0 {
			rs := make([]map[string]any, 0, len(reports))
			for _, r := range reports {
				rs = append(rs, map[string]any{"kind": string(r.Kind), "summary": r.Summary})
			}
			entry["reports"] = rs
		}
		if p.resolver != nil {
			if asks, err := p.resolver.PendingAsks(ctx, m.ID); err == nil && len(asks) > 0 {
				entry["waitingOnYou"] = asks
			}
		}
		out = append(out, entry)
	}
	raw, err := json.Marshal(map[string]any{"missions": out})
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: encode fired missions: %w", err)
	}
	return string(raw), taskengine.DataTypeString, nil
}

const (
	supervisorMissionLimit = 20
	supervisorReportLimit  = 5
)

func (p *provider) execAnswer(ctx context.Context, parentSessionID string, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if p.resolver == nil || p.supervisor == nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: mission supervision is not wired in this process")
	}
	askID := strings.TrimSpace(argString(input, call, "askId"))
	answer := strings.TrimSpace(argString(input, call, "answer"))
	if askID == "" || answer == "" {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %s requires askId and answer", ToolNameAnswer)
	}

	missions, err := p.supervisor.MissionsFiredBy(ctx, parentSessionID, supervisorMissionLimit)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: resolve your subagents: %w", err)
	}
	for _, m := range missions {
		asks, err := p.resolver.PendingAsks(ctx, m.ID)
		if err != nil {
			continue
		}
		for _, ask := range asks {
			if ask.AskID != askID {
				continue
			}
			if err := p.resolver.AnswerAsAgent(ctx, askID, answer); err != nil {
				var refused *AnswerRefusedError
				if errors.As(err, &refused) {
					// A result, not an error, so the model stops rather than retrying a denial that cannot change.
					return fmt.Sprintf("answer denied per policy for ask %s.", askID), taskengine.DataTypeString, nil
				}
				return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: answer %s: %w", askID, err)
			}
			return fmt.Sprintf("answered %s — subagent %q has your reply and continues", askID, m.AgentName), taskengine.DataTypeString, nil
		}
	}
	return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %q is not a question your subagents are currently waiting on (already answered, expired, or not yours)", askID)
}
