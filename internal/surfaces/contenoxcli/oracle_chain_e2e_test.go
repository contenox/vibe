package contenoxcli

// The oracle chain executed end to end against a scripted model: the shipped
// chain-oracle-default.json runs on the real task engine with the real
// oracletools provider, and the script drives every failure mode the loop
// must recover from — chat text, wrong askId, malformed shape, a policy
// denial — plus the budget-exhaustion WAIT-equivalent ending.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/llmrepo"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// scriptedTurn is one model reply: chat text, or one submit_verdict call.
type scriptedTurn struct {
	text string
	args string // non-empty: a tool call to oracle.submit_verdict with these arguments
}

// scriptedRepo replays turns and records the messages each Chat call saw.
type scriptedRepo struct {
	mu    sync.Mutex
	turns []scriptedTurn
	seen  [][]libmodelprovider.Message
}

func (r *scriptedRepo) Tokenize(context.Context, string, string) ([]int, error) { return []int{1}, nil }
func (r *scriptedRepo) CountTokens(context.Context, string, string) (int, error) {
	return 1, nil
}
func (r *scriptedRepo) PromptExecute(context.Context, llmrepo.Request, string, float32, string) (string, llmrepo.Meta, error) {
	return "", llmrepo.Meta{}, errors.New("scripted: prompt path unused")
}
func (r *scriptedRepo) Embed(context.Context, llmrepo.EmbedRequest, string) ([]float64, llmrepo.Meta, error) {
	return nil, llmrepo.Meta{}, errors.New("scripted: embed path unused")
}
func (r *scriptedRepo) Stream(context.Context, llmrepo.Request, []libmodelprovider.Message, ...libmodelprovider.ChatArgument) (<-chan *libmodelprovider.StreamParcel, llmrepo.Meta, error) {
	return nil, llmrepo.Meta{}, errors.New("scripted: stream path unused")
}

func (r *scriptedRepo) Chat(_ context.Context, _ llmrepo.Request, msgs []libmodelprovider.Message, _ ...libmodelprovider.ChatArgument) (libmodelprovider.ChatResult, llmrepo.Meta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, append([]libmodelprovider.Message(nil), msgs...))
	if len(r.turns) == 0 {
		return libmodelprovider.ChatResult{}, llmrepo.Meta{}, errors.New("scripted: script exhausted")
	}
	turn := r.turns[0]
	r.turns = r.turns[1:]
	res := libmodelprovider.ChatResult{
		Message:      libmodelprovider.Message{Role: "assistant", Content: turn.text},
		FinishReason: "stop",
	}
	if turn.args != "" {
		tc := libmodelprovider.ToolCall{ID: fmt.Sprintf("call-%d", len(r.seen)), Type: "function"}
		tc.Function.Name = "oracle." + oracletools.ToolNameSubmitVerdict
		tc.Function.Arguments = turn.args
		res.ToolCalls = []libmodelprovider.ToolCall{tc}
	}
	return res, llmrepo.Meta{}, nil
}

// sawUserContaining reports whether any recorded Chat call past callIdx
// carried a user/tool message containing needle.
func (r *scriptedRepo) sawMessageContaining(needle string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, msgs := range r.seen {
		for _, m := range msgs {
			if strings.Contains(m.Content, needle) {
				return true
			}
		}
	}
	return false
}

func (r *scriptedRepo) chatCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

const e2eAskID = "ask-e2e-1"

// runOracleChain executes the embedded default oracle chain over the
// scripted repo and the real oracle tools provider.
func runOracleChain(t *testing.T, repo *scriptedRepo, answerer oracletools.Answerer) (*oracletools.AskBinding, error) {
	t.Helper()
	ctx := context.Background()

	chain := loadOracleChain(t)
	tools := oracletools.New(answerer)

	exec, err := taskengine.NewExec(ctx, repo, tools, libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(ctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), tools)
	require.NoError(t, err)
	env, err = taskengine.NewMacroEnv(env, tools)
	require.NoError(t, err)

	input := fmt.Sprintf(`{"missionId":"m-e2e","askId":%q,"agentName":"agent-x","intent":"write the haiku","summary":"proceed?","detail":"the intent pre-authorizes yes"}`, e2eAskID)
	binding := oracletools.NewAskBinding(e2eAskID, input)
	runCtx := oracletools.WithBinding(ctx, binding)
	runCtx = taskengine.WithTemplateVars(runCtx, map[string]string{
		"model": "test-model", "provider": "testprov", "think": "off",
		"default_model": "test-model", "default_provider": "testprov",
	})

	_, _, _, execErr := env.ExecEnv(runCtx, chain, input, taskengine.DataTypeString)
	return binding, execErr
}

func loadOracleChain(t *testing.T) *taskengine.TaskChainDefinition {
	t.Helper()
	chain, _ := oracleChainByID(t, initOracleDefaultChain)
	return &chain
}

// TestUnit_OracleChainE2E_HappyPathAnswersInTwoTurns pins the clean contract:
// one valid call, one DONE, the answer delivered exactly once.
func TestUnit_OracleChainE2E_HappyPathAnswersInTwoTurns(t *testing.T) {
	repo := &scriptedRepo{turns: []scriptedTurn{
		{args: fmt.Sprintf(`{"verdict":"answer","answer":"Yes, proceed.","askId":%q}`, e2eAskID)},
		{text: "DONE"},
	}}
	answerer := &recordingChainAnswerer{}
	binding, err := runOracleChain(t, repo, answerer)
	require.NoError(t, err)
	require.Equal(t, oracletools.OutcomeAnswered, binding.Outcome())
	require.Equal(t, []string{"Yes, proceed."}, answerer.texts)
	require.Equal(t, 2, repo.chatCalls())
}

// TestUnit_OracleChainE2E_MultiRoundRecoveryWithinBudget drives the founder
// matrix in one run: chat text (corrective nudge), a wrong askId, a malformed
// verdict — each answered by an observable corrective result — and a valid
// call landing on a later round, within the budgets.
func TestUnit_OracleChainE2E_MultiRoundRecoveryWithinBudget(t *testing.T) {
	repo := &scriptedRepo{turns: []scriptedTurn{
		{text: "I judge this routine; the answer is yes."},                                        // chat text → nudge
		{args: fmt.Sprintf(`{"verdict":"answer","answer":"yes","askId":"ask-imagined"}`)},         // wrong askId → corrective
		{args: fmt.Sprintf(`{"verdict":"approve","askId":%q}`, e2eAskID)},                         // malformed verdict → corrective
		{args: fmt.Sprintf(`{"verdict":"answer","answer":"Yes, proceed.","askId":%q}`, e2eAskID)}, // recovered
		{text: "DONE"},
		{text: "DONE"}, // the tool budget routes the last text through recovery
	}}
	answerer := &recordingChainAnswerer{}
	binding, err := runOracleChain(t, repo, answerer)
	require.NoError(t, err)
	require.Equal(t, oracletools.OutcomeAnswered, binding.Outcome(), "the loop recovered to a valid verdict within budget")
	require.Equal(t, []string{"Yes, proceed."}, answerer.texts, "exactly one delivery")

	require.True(t, repo.sawMessageContaining("output rejected: submit the verdict via submit_verdict"),
		"the chat-text round got the machine-register nudge")
	require.True(t, repo.sawMessageContaining("askId must be the askId field of the INPUT event"),
		"the wrong-askId round got the corrective naming the expected source field")
	require.True(t, repo.sawMessageContaining("invalid verdict"),
		"the malformed round got the exact-schema corrective")
	require.True(t, repo.sawMessageContaining(`"accepted":false`),
		"every rejection reaches the model as the declared accepted=false response")
}

// TestUnit_OracleChainE2E_NudgeBudgetExhaustionEndsWait pins the bounded
// text-only degenerate: two corrections, then the correction budget ends the
// chain WAIT-equivalent with nothing executed.
func TestUnit_OracleChainE2E_NudgeBudgetExhaustionEndsWait(t *testing.T) {
	repo := &scriptedRepo{turns: []scriptedTurn{
		{text: "chat text instead of a verdict"},
		{text: "still chat text"},
		{text: "and again"},
	}}
	answerer := &recordingChainAnswerer{}
	binding, err := runOracleChain(t, repo, answerer)
	require.NoError(t, err, "budget exhaustion is a clean WAIT-equivalent end, not a chain error")
	require.Equal(t, oracletools.OutcomeNone, binding.Outcome())
	require.Empty(t, answerer.texts, "nothing was executed")
	require.Equal(t, 3, repo.chatCalls(), "two corrections, then the budget ends it")
}

// TestUnit_OracleChainE2E_WaitVerdictExecutesNothing pins WAIT: settled,
// acknowledged, no delivery.
func TestUnit_OracleChainE2E_WaitVerdictExecutesNothing(t *testing.T) {
	repo := &scriptedRepo{turns: []scriptedTurn{
		{args: fmt.Sprintf(`{"verdict":"wait","askId":%q}`, e2eAskID)},
		{text: "DONE"},
	}}
	answerer := &recordingChainAnswerer{}
	binding, err := runOracleChain(t, repo, answerer)
	require.NoError(t, err)
	require.Equal(t, oracletools.OutcomeWait, binding.Outcome())
	require.Empty(t, answerer.texts)
}

// TestUnit_OracleChainE2E_PolicyDenialThenWait pins the denial contract at
// chain level: the plain denied-per-policy result reaches the model, and the
// scripted follow-up wait settles the contract cleanly — no answer delivered.
func TestUnit_OracleChainE2E_PolicyDenialThenWait(t *testing.T) {
	repo := &scriptedRepo{turns: []scriptedTurn{
		{args: fmt.Sprintf(`{"verdict":"answer","answer":"yes","askId":%q}`, e2eAskID)},
		{args: fmt.Sprintf(`{"verdict":"wait","askId":%q}`, e2eAskID)},
		{text: "DONE"},
	}}
	answerer := &recordingChainAnswerer{err: &oracletools.AnswerRefusedError{Reason: "envelope forbids"}}
	binding, err := runOracleChain(t, repo, answerer)
	require.NoError(t, err)
	require.Equal(t, oracletools.OutcomeWait, binding.Outcome(), "the denial never settles; the wait does")
	require.Equal(t, []string{"yes"}, answerer.attempts, "the delivery was attempted once and denied")
	require.Empty(t, answerer.texts, "nothing was delivered")
	require.True(t, repo.sawMessageContaining("answer denied per policy for ask "+e2eAskID),
		"the model saw the plain denial, nothing more")
}

// recordingChainAnswerer records delivered texts; err (when set) denies every
// delivery and records it as an attempt instead.
type recordingChainAnswerer struct {
	mu       sync.Mutex
	texts    []string
	attempts []string
	err      error
}

func (a *recordingChainAnswerer) Answer(_ context.Context, _ string, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		a.attempts = append(a.attempts, text)
		return a.err
	}
	a.texts = append(a.texts, text)
	return nil
}
