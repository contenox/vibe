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

// The supervisor half of this package's tool surface: what a session that
// fired missions may see and do about them. Distinct from the unit's own
// tools (report/ask/plan/finish) and gated on a different fact — unit tools
// unlock for a session that IS a mission, these unlock for a session that
// HAS missions. It exists because `/mission` dispatches without the firing
// session seeing it happen, leaving it unable to check status or answer a
// unit's question without these tools.
const (
	// ToolNameListMissions lists the missions this session fired.
	ToolNameListMissions = "mission_list"
	// ToolNameAnswer answers a question raised by one of this session's missions.
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

// AttentionResolver is the answering half of the supervisor surface: find
// what a mission is waiting on, and answer it. Kept separate from
// SupervisorStore since the two live in different services.
//
// AnswerAsAgent must enforce the mission envelope's agent-answer bounds: a
// session agent can reach a live askId without ever being offered the
// question, so the write is where the bound has to hold. An
// *AnswerRefusedError says the envelope held; any other error is plumbing.
type AttentionResolver interface {
	// PendingAsks returns the unanswered questions for missionID.
	PendingAsks(ctx context.Context, missionID string) ([]PendingAsk, error)
	// AnswerAsAgent resolves askID with text, recorded as answered by an
	// agent rather than a human — a distinction the envelope's cap counts on.
	AnswerAsAgent(ctx context.Context, askID, text string) error
}

// AnswerRefusedError is the mission envelope declining an agent answer — a
// refusal, not broken plumbing. This package discards Reason (the model sees
// the plain denial only), so the resolver must state it on the operator's
// trace at the refusal point or the denial is invisible.
type AnswerRefusedError struct{ Reason string }

func (e *AnswerRefusedError) Error() string { return e.Reason }

// WithParentSessionID marks ctx as belonging to a session that may supervise
// its own missions. The mirror of WithMissionID: that says "you ARE a
// mission", this says "you HAVE missions" — a session is never both.
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

// supervisorTools are the schemas offered to a firing session.
func supervisorTools() []taskengine.Tool {
	return []taskengine.Tool{listMissionsToolSchema(), answerToolSchema()}
}

func listMissionsToolSchema() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name: ToolNameListMissions,
			Description: "List the missions YOU fired from this session — their intent, status, last heartbeat, latest reports, and any QUESTION a unit is currently waiting on you to answer. " +
				"Call it when you need to know what your subagents are doing, before answering the user about them, or to get the `askId` you need for " + ToolNameAnswer + ".",
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
			Description: "Answer a QUESTION one of your own mission units is blocked on. The unit is parked waiting for this: your text becomes the result of the tool call it asked with, and it continues immediately. " +
				"Get `askId` from " + ToolNameListMissions + " (or from the question you were just shown). Answer only what you actually know — if the answer needs the user, ask THEM first and relay it; a confident wrong answer sends the unit down the wrong path.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"askId": map[string]any{
						"type":        "string",
						"description": "The id of the question to answer.",
					},
					"answer": map[string]any{
						"type":        "string",
						"description": "The answer, in your own words — what the unit needs to proceed.",
					},
				},
				"required": []string{"askId", "answer"},
			},
		},
	}
}

// execListMissions answers "what did I dispatch, and does anything need me?"
// for the calling session, folding in pending questions rather than
// requiring a second call.
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

// execAnswer resolves one of the caller's own units' questions. Ownership is
// checked, not assumed: the ask must belong to a mission this session fired,
// since an answer is an instruction the unit acts on immediately, not a read.
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
		return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: resolve your missions: %w", err)
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
					// The envelope holding, nothing more: a plain result, so
					// the model stops rather than retrying a denial that
					// cannot change. The reason stays on the operator's trace.
					return fmt.Sprintf("answer denied per policy for ask %s.", askID), taskengine.DataTypeString, nil
				}
				return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: answer %s: %w", askID, err)
			}
			return fmt.Sprintf("answered %s — unit %q has your reply and continues", askID, m.AgentName), taskengine.DataTypeString, nil
		}
	}
	// Not found among the caller's own pending asks: not theirs, or already
	// resolved. Both get one honest message — stop trying.
	return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %q is not a question your missions are currently waiting on (already answered, expired, or not yours)", askID)
}
