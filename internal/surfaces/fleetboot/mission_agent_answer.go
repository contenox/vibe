package fleetboot

import (
	"context"
	"errors"
	"fmt"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/surfaces/acpsvc"
)

// The tool the supervisor is told to call, named from the tool package's own
// constants so the instruction cannot drift from what the model is actually
// offered — the same drift that once left units calling functions that did not
// exist.
var (
	missionToolsProviderName = missiontools.ToolsProviderName
	missionAnswerToolName    = missiontools.ToolNameAnswer
)

// sessionPrompter is the narrow capability this needs from the ACP layer: run one
// out-of-band turn on a live session. Both shapes of that surface satisfy it —
// the editor's lone *acpsvc.Transport and serve's *acpsvc.SessionRouter, which
// finds the right one of its many connections.
type sessionPrompter interface {
	PromptContenoxSession(ctx context.Context, contenoxSessionID, text string) error
}

// agentAnswerOffer decides whether a unit's question should be put to the AGENT
// driving the session that fired it, and does it.
//
// This is the autonomous edge of mission mode, so what it refuses matters more
// than what it does. A unit calls mission_ask_attention precisely because it hit
// something it must not decide alone; answering that with another model is only
// legitimate when the operator declared it so. Hence, in order:
//
//   - the mission's ENVELOPE must permit agent answers (default: it does not);
//   - the per-mission CAP must not be spent — counted on the durable rows, so a
//     restart cannot reset a runaway loop's budget;
//   - the parent session must be LIVE and IDLE (an agent-to-agent exchange never
//     interleaves with something the operator is in the middle of).
//
// Any refusal is silent and normal: the question is already in the operator's
// queue and in their session, so declining costs a human a reply, never the
// question itself.
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
		// Without the mission there is no envelope, and without an envelope there is
		// no permission. Human-only.
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
		// The loop bound: a supervisor that has already answered this unit `cap`
		// times has not unstuck it, and the next question is the one a human should
		// see.
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

// agentAnswerPrompt frames the unit's question for its supervisor. It names the
// tool and the ask id explicitly, because the model cannot see the `_meta` the
// client renders the question card from — and it says plainly what NOT knowing
// looks like, since a supervisor inventing an answer is worse for the mission
// than one that admits it must ask the human.
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
