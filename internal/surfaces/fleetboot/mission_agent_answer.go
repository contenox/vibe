package fleetboot

import (
	"context"
	"errors"
	"fmt"

	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
)

// Mirrors missiontools' own constants so the instruction given to the model
// cannot drift from the tool actually offered.
var (
	missionToolsProviderName = missiontools.ToolsProviderName
	missionAnswerToolName    = missiontools.ToolNameAnswer
)

// sessionPrompter runs one out-of-band turn on a live session; satisfied by
// both *acpsvc.Transport and *acpsvc.SessionRouter.
type sessionPrompter interface {
	PromptContenoxSession(ctx context.Context, contenoxSessionID, text string) error
}

// agentAnswerOffer decides whether a unit's question should go to the agent
// driving the session that fired it, and does it.
//
// Requires, in order: the mission's envelope permits agent answers (default:
// it does not), the per-mission answer cap is not spent (counted on durable
// rows, survives restarts), and the parent session is live and idle. Any
// refusal is silent — the question stays queued for the human either way.
type agentAnswerOffer struct {
	hitl     hitlservice.Service
	missions missionservice.Service
	prompter sessionPrompter
	tracker  libtracker.ActivityTracker
}

// OfferToSupervisingAgent implements reportrouter.AgentSupervisor.
func (a agentAnswerOffer) OfferToSupervisingAgent(ctx context.Context, ev missionservice.AttentionAskedEvent) error {
	if a.prompter == nil || a.hitl == nil || a.missions == nil || ev.ParentSessionID == "" {
		return nil
	}
	reportErr, reportChange, end := a.tracker.Start(ctx, "offer", "mission_agent_answer",
		"mission_id", ev.MissionID, "ask_id", ev.AskID)
	defer end()

	m, err := a.missions.Get(ctx, ev.MissionID)
	if err != nil || m == nil {
		// No mission means no envelope means no permission: human-only.
		reportChange("declined", "mission_unreadable")
		return nil
	}
	bounds, err := a.hitl.AttentionBoundsFor(ctx, m.HITLPolicyName)
	if err != nil {
		reportChange("declined", "envelope_unreadable")
		return nil
	}
	if !bounds.AllowAgentAnswers {
		reportChange("declined", "envelope_forbids")
		return nil
	}
	used, err := a.hitl.AgentAnswerCount(ctx, ev.MissionID)
	if err != nil {
		reportChange("declined", "cap_uncountable")
		return nil
	}
	if cap := bounds.EffectiveMaxAgentAnswers(); used >= cap {
		// cap reached: a supervisor that hasn't unstuck the unit by now should
		// yield to a human.
		reportChange("declined", fmt.Sprintf("cap_reached(%d/%d)", used, cap))
		return nil
	}

	if err := a.prompter.PromptContenoxSession(ctx, ev.ParentSessionID, agentAnswerPrompt(ev)); err != nil {
		switch {
		case errors.Is(err, acpsvc.ErrSessionBusy):
			reportChange("declined", "session_busy")
		case errors.Is(err, acpsvc.ErrSessionNotLive):
			reportChange("declined", "session_not_live")
		default:
			reportErr(err)
		}
		return nil
	}
	reportChange("offered", "prompted_supervising_agent")
	return nil
}

// agentAnswerPrompt frames the unit's question for its supervisor, naming the
// tool and ask id explicitly since the model cannot see the client's `_meta`
// question card.
func agentAnswerPrompt(ev missionservice.AttentionAskedEvent) string {
	unit := ev.AgentName
	if unit == "" {
		unit = "a mission unit"
	}
	detail := ""
	if ev.Detail != "" {
		detail = "\n\nIts detail: " + ev.Detail
	}
	return fmt.Sprintf(
		"Your mission unit %q (mission %s, intent %q) is BLOCKED waiting on an answer from you.\n\n"+
			"Its question: %s%s\n\n"+
			"If you know the answer from this conversation, give it by calling %s.%s with askId %q — the unit is parked on that call and continues the moment you answer. "+
			"If you do NOT know, do not guess: say so briefly to the user and let them answer it themselves.",
		unit, ev.MissionID, ev.Intent, ev.Summary, detail,
		missionToolsProviderName, missionAnswerToolName, ev.AskID,
	)
}
