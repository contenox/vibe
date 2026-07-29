package fleetboot

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/stretchr/testify/require"
)

// recordingPrompter records whether the supervising agent was asked.
type recordingPrompter struct {
	prompts []string
	err     error
}

func (p *recordingPrompter) PromptContenoxSession(_ context.Context, _, text string) error {
	if p.err != nil {
		return p.err
	}
	p.prompts = append(p.prompts, text)
	return nil
}

// stubHITL answers the two questions the offer asks: envelope bounds and
// agent-answer count.
type stubHITL struct {
	hitlservice.Service
	bounds hitlservice.AttentionBounds
	used   int
	err    error
}

func (s stubHITL) AttentionBoundsFor(context.Context, string) (hitlservice.AttentionBounds, error) {
	return s.bounds, s.err
}

func (s stubHITL) AgentAnswerCount(context.Context, string) (int, error) { return s.used, s.err }

// stubMissions returns one mission; the envelope name is all the offer reads.
type stubMissions struct {
	missionservice.Service
	mission *missionservice.Mission
	err     error
}

func (s stubMissions) Get(context.Context, string) (*missionservice.Mission, error) {
	return s.mission, s.err
}

func offerFixture(t *testing.T, bounds hitlservice.AttentionBounds, used int, prompter *recordingPrompter) agentAnswerOffer {
	t.Helper()
	return agentAnswerOffer{
		hitl:     stubHITL{bounds: bounds, used: used},
		missions: stubMissions{mission: &missionservice.Mission{ID: "m-1", HITLPolicyName: "envelope.json"}},
		prompter: prompter,
		tracker:  libtracker.NoopTracker{},
	}
}

func askEvent() missionservice.AttentionAskedEvent {
	return missionservice.AttentionAskedEvent{
		MissionID: "m-1", AskID: "ask-1", ParentSessionID: "cnx-parent",
		AgentName: "chain-acp", Intent: "explain the moat", Summary: "which project?",
	}
}

// TestUnit_AgentAnswer_DefaultEnvelopeKeepsItHuman pins: no model answers unless the envelope opts in.
func TestUnit_AgentAnswer_DefaultEnvelopeKeepsItHuman(t *testing.T) {
	prompter := &recordingPrompter{}
	offer := offerFixture(t, hitlservice.AttentionBounds{}, 0, prompter)

	require.NoError(t, offer.OfferToSupervisingAgent(context.Background(), askEvent()))
	require.Empty(t, prompter.prompts, "the default envelope must not let an agent answer")
}

// TestUnit_AgentAnswer_AllowedEnvelopePutsItToTheAgent pins the prompt content: unit, question, tool call, and ask id.
func TestUnit_AgentAnswer_AllowedEnvelopePutsItToTheAgent(t *testing.T) {
	prompter := &recordingPrompter{}
	offer := offerFixture(t, hitlservice.AttentionBounds{AllowAgentAnswers: true}, 0, prompter)

	require.NoError(t, offer.OfferToSupervisingAgent(context.Background(), askEvent()))
	require.Len(t, prompter.prompts, 1)
	prompt := prompter.prompts[0]
	require.Contains(t, prompt, "which project?")
	require.Contains(t, prompt, "chain-acp")
	require.Contains(t, prompt, "mission.mission_answer", "the supervisor must be told the tool as the model sees it")
	require.Contains(t, prompt, "ask-1", "…and the handle to answer with")
	require.Contains(t, prompt, "do not guess", "a supervisor that invents an answer is worse than one that defers")
}

// TestUnit_AgentAnswer_CapEndsTheLoop pins: past the envelope's cap, the next question goes to a human.
func TestUnit_AgentAnswer_CapEndsTheLoop(t *testing.T) {
	prompter := &recordingPrompter{}
	bounds := hitlservice.AttentionBounds{AllowAgentAnswers: true, MaxAgentAnswers: 2}
	offer := offerFixture(t, bounds, 2, prompter)

	require.NoError(t, offer.OfferToSupervisingAgent(context.Background(), askEvent()))
	require.Empty(t, prompter.prompts, "a spent cap stops the exchange")
}

// TestUnit_AgentAnswer_BusySessionDeclines pins: a busy parent session declines without erroring.
func TestUnit_AgentAnswer_BusySessionDeclines(t *testing.T) {
	prompter := &recordingPrompter{err: acpsvc.ErrSessionBusy}
	offer := offerFixture(t, hitlservice.AttentionBounds{AllowAgentAnswers: true}, 0, prompter)

	require.NoError(t, offer.OfferToSupervisingAgent(context.Background(), askEvent()))
}

// TestUnit_AgentAnswer_UnreadableEnvelopeStaysHuman pins the fail-safe: an unreadable envelope never defaults to yes.
func TestUnit_AgentAnswer_UnreadableEnvelopeStaysHuman(t *testing.T) {
	prompter := &recordingPrompter{}
	offer := agentAnswerOffer{
		hitl:     stubHITL{err: errors.New("policy source down")},
		missions: stubMissions{mission: &missionservice.Mission{ID: "m-1"}},
		prompter: prompter,
		tracker:  libtracker.NoopTracker{},
	}

	require.NoError(t, offer.OfferToSupervisingAgent(context.Background(), askEvent()))
	require.Empty(t, prompter.prompts)
}

// TestUnit_AgentAnswer_OperatorFiredMissionIsNotOffered pins: no firing session means no agent to ask.
func TestUnit_AgentAnswer_OperatorFiredMissionIsNotOffered(t *testing.T) {
	prompter := &recordingPrompter{}
	offer := offerFixture(t, hitlservice.AttentionBounds{AllowAgentAnswers: true}, 0, prompter)

	ev := askEvent()
	ev.ParentSessionID = ""
	require.NoError(t, offer.OfferToSupervisingAgent(context.Background(), ev))
	require.Empty(t, prompter.prompts)
}

var (
	_ = runtimetypes.LocalTenantID
	_ = taskengine.NoopTaskEventSink{}
)
