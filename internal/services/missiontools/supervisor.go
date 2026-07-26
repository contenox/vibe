package missiontools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/missionservice"
)

// The SUPERVISOR half of this package's tool surface: what a session that FIRED
// missions may see and do about them. It is deliberately a different set from the
// unit's own (report/ask/plan/finish) and gated on a different fact — the unit
// tools unlock for the session that IS a mission, these unlock for a session that
// HAS missions.
//
// It exists because a firing session was blind by construction. `/mission` is a
// slash command the transport handles, so the agent driving that session never saw
// the dispatch happen; the only trace it got was the confirmation text in its
// history, and it had no way to ask what became of the unit or to answer a
// question the unit escalated. A supervisor that can only be told things, never
// look or reply, is not supervising.
const (
	// ToolNameListMissions lists the missions THIS session fired.
	ToolNameListMissions = "mission_list"
	// ToolNameAnswer answers a question raised by one of THIS session's missions.
	ToolNameAnswer = "mission_answer"
)

// SupervisorStore is what a firing session is allowed to read: its OWN missions,
// nothing else. Narrow on purpose — a supervisor has no business listing another
// session's work, so the lookup is by parent session id rather than a general
// list with a filter the caller could widen.
type SupervisorStore interface {
	// MissionsFiredBy returns the missions whose ParentSessionID is
	// parentSessionID, newest first.
	MissionsFiredBy(ctx context.Context, parentSessionID string, limit int) ([]*missionservice.Mission, error)
	// ListReports returns a mission's reports, newest first — what the unit has
	// said so far, which is most of what "how is it going" means.
	ListReports(ctx context.Context, missionID string, limit int) ([]*missionservice.Report, error)
}

// PendingAsk is one unanswered question from a unit, as a supervisor sees it: the
// question, and the handle to answer it with.
type PendingAsk struct {
	AskID     string `json:"askId"`
	MissionID string `json:"missionId"`
	Question  string `json:"question"`
	Detail    string `json:"detail,omitempty"`
	AskedAt   string `json:"askedAt,omitempty"`
}

// AttentionResolver is the answering half of the supervisor surface: find what a
// mission is waiting on, and answer it. Kept separate from SupervisorStore
// because the two live in different services (missions vs the durable ask store),
// and this package refuses to know that.
type AttentionResolver interface {
	// PendingAsks returns the unanswered questions for missionID.
	PendingAsks(ctx context.Context, missionID string) ([]PendingAsk, error)
	// AnswerAsAgent resolves askID with text, recorded as answered by an AGENT
	// rather than by a human — the distinction the envelope's cap is counted on.
	AnswerAsAgent(ctx context.Context, askID, text string) error
}

// WithParentSessionID marks ctx as belonging to a session that may supervise its
// own missions, unlocking the supervisor tools for it. It is the mirror of
// WithMissionID: that one says "you ARE a mission", this one says "you HAVE
// missions", and a session is never both (a dispatched unit does not fire its own
// subagents today).
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

// execListMissions answers "what did I dispatch, and does anything need me?" for
// the calling session. The pending questions are folded in rather than left to a
// second call: a supervisor that has to discover it must ask again usually does
// not.
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

// execAnswer resolves one of the caller's own units' questions.
//
// Ownership is checked, not assumed: the ask must belong to a mission THIS session
// fired. Without that check a session could answer another session's unit by
// guessing an id — and an answer is not a read, it is an instruction the unit acts
// on immediately.
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
				return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: answer %s: %w", askID, err)
			}
			return fmt.Sprintf("answered %s — unit %q has your reply and continues", askID, m.AgentName), taskengine.DataTypeString, nil
		}
	}
	// Not found among the caller's own pending asks: either it is not theirs to
	// answer, or it was already answered (by a human in the inbox, or by its own
	// timeout). Both are the same instruction to the model — stop trying — so they
	// get one honest message rather than a probe that distinguishes them.
	return nil, taskengine.DataTypeAny, fmt.Errorf("missiontools: %q is not a question your missions are currently waiting on (already answered, expired, or not yours)", askID)
}
