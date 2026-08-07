// oracle_driver.go is the oracle attention driver `mission fire --oracle`
// mounts: a reportrouter.AgentSupervisor — the a2a firing-agent offer's
// sibling on the same seam — that reviews an operator-fired mission's
// attention ask by running the oracle chain in-process on the host's engine.
// A valid ANSWER verdict is delivered through the service layer
// (hitlservice.EnforceAgentAnswerBounds + AnswerAsAgentNamed as "oracle");
// WAIT, malformed-after-guidance, a bounds refusal, a chain error, or budget
// exhaustion changes nothing — the untouched normal path (park → checkpoint →
// durable ask → human) proceeds.
package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/contenox/contenox/internal/services/reportrouter"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

// oracleAgentName is the actor recorded on an oracle-delivered answer
// (answeredBy on the durable resolution) and its agent-answer-count identity.
const oracleAgentName = "oracle"

// oraclePolicyName pins the oracle chain's own execution envelope.
const oraclePolicyName = "hitl-policy-oracle.json"

// oracleAnswerer adapts the service layer to oracletools.Answerer: the same
// atomic bounded delivery (hitlservice.AnswerAsAgentWithinBounds) that
// `approvals respond --as-agent` and the `mission_answer` tool run, so the
// three together spend at most the envelope's cap. Every terminal holding —
// the envelope's bounds, or the ask already gone — maps to the typed refusal
// the tool contract never retries, and states its reason on the operator's
// trace (out) the way the firing-agent offer's declines do.
type oracleAnswerer struct {
	hitl     hitlservice.Service
	missions missionservice.Service
	store    runtimetypes.Store
	// out receives one line per refusal — the ONLY place a denial's reason is
	// ever stated. The model-facing result stays the plain denial.
	out io.Writer
}

var _ oracletools.Answerer = oracleAnswerer{}

// refuse writes the operator line for reason and returns the typed refusal;
// the tool renders that refusal as the plain denied-per-policy result, reason
// discarded.
func (a oracleAnswerer) refuse(askID, reason string) error {
	if a.out != nil {
		fmt.Fprintf(a.out, "oracle: answer refused for ask %s: %s\n", askID, reason)
	}
	return &oracletools.AnswerRefusedError{Reason: reason}
}

func (a oracleAnswerer) Answer(ctx context.Context, askID, text string) error {
	row, err := a.store.GetHITLApproval(ctx, askID)
	if err != nil {
		if errors.Is(err, libdbexec.ErrNotFound) {
			return a.refuse(askID, fmt.Sprintf("ask %s no longer exists", askID))
		}
		return fmt.Errorf("read ask %s: %w", askID, err)
	}
	if !hitlservice.IsAttentionAsk(row) {
		return a.refuse(askID, fmt.Sprintf("ask %s is a permission request, not a question", askID))
	}
	if err := hitlservice.AnswerAsAgentWithinBounds(ctx, a.missions, a.hitl, row, oracleAgentName, text); err != nil {
		switch {
		case hitlservice.IsAgentAnswerRefusal(err),
			errors.Is(err, hitlservice.ErrApprovalNotFound),
			errors.Is(err, hitlservice.ErrApprovalAlreadyResolved),
			errors.Is(err, hitlservice.ErrApprovalExpired):
			return a.refuse(askID, err.Error())
		default:
			return err
		}
	}
	return nil
}

// oracleAttentionDriver runs one oracle-chain review per offered ask, for the
// missions this host itself dispatched (see OfferToSupervisingAgent).
type oracleAttentionDriver struct {
	// mu guards owned: Dispatch registers on the command goroutine while the
	// bus subscriber reads on the router's.
	mu    sync.Mutex
	owned map[string]bool

	agent         agentservice.Agent
	chain         *taskengine.TaskChainDefinition
	chainRef      string
	templateVars  map[string]string
	contextLength int
	// out receives the driver's one-line trace per review.
	out io.Writer
}

var _ reportrouter.AgentSupervisor = (*oracleAttentionDriver)(nil)

// own registers a mission this host dispatched, so its asks become eligible
// for review. Called after Dispatch returns; an ask arriving before the
// registration lands is declined, which is correct — the driver only ever
// declines to answer, never answers wrongly.
func (d *oracleAttentionDriver) own(missionID string) {
	if missionID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.owned == nil {
		d.owned = map[string]bool{}
	}
	d.owned[missionID] = true
}

// owns reports whether missionID was dispatched by this host.
func (d *oracleAttentionDriver) owns(missionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.owned[missionID]
}

// OfferToSupervisingAgent implements reportrouter.AgentSupervisor. A
// parented ask is the firing-agent offer's territory; the driver takes only
// operator-fired asks. It never returns an error: every non-answer outcome is
// a decline, and the ask is durable either way.
//
// The ownership check is not redundant with parentage: every host's
// reportrouter subscribes to the SHARED bus, so without it one
// `mission fire --oracle` would review — and spend a model call on — asks
// raised by missions fired from other terminals whose operator never passed
// --oracle. The envelope still gates the write, so this bounds scope and
// spend, not authority.
func (d *oracleAttentionDriver) OfferToSupervisingAgent(ctx context.Context, ev missionservice.AttentionAskedEvent) error {
	if ev.ParentSessionID != "" || ev.AskID == "" || !d.owns(ev.MissionID) {
		return nil
	}
	input, err := json.Marshal(ev)
	if err != nil {
		return nil
	}

	binding := oracletools.NewAskBinding(ev.AskID, string(input))
	runCtx := libtracker.WithNewRequestID(ctx)
	runCtx = hitlservice.WithPolicyName(runCtx, oraclePolicyName)
	runCtx = oracletools.WithBinding(runCtx, binding)

	d.tracef("oracle: reviewing ask %s (mission %s): %s", ev.AskID, ev.MissionID, ev.Summary)
	start := time.Now()
	_, runErr := d.agent.Prompt(runCtx, agentservice.PromptRequest{
		Input:         string(input),
		InputValue:    string(input),
		InputType:     taskengine.DataTypeString,
		Chain:         d.chain,
		ChainRef:      d.chainRef,
		TemplateVars:  d.templateVars,
		ContextLength: d.contextLength,
	})
	elapsed := time.Since(start).Round(time.Millisecond)

	switch binding.Outcome() {
	case oracletools.OutcomeAnswered:
		// In-window: the parked unit's poll picks the resolved row up and
		// continues with NO checkpoint. Past the window the unit already
		// checkpointed, and the delivery went through the durable respond
		// path, whose resume hook resumed the run — the human-style late case.
		late := ""
		if elapsed > missiontools.AttentionParkWindow {
			late = " (past the park window: the ask had checkpointed; the durable respond path resumed it)"
		}
		d.tracef("oracle: answered ask %s in %s as agent %q: %q%s", ev.AskID, elapsed, oracleAgentName, binding.Answer(), late)
	case oracletools.OutcomeWait:
		d.tracef("oracle: WAIT for ask %s (%s) — the question stays with a human", ev.AskID, elapsed)
	default:
		if runErr != nil {
			d.tracef("oracle: chain error for ask %s (%s): %v — the question stays with a human", ev.AskID, elapsed, runErr)
		} else {
			d.tracef("oracle: no verdict for ask %s within the chain budgets (%s) — the question stays with a human", ev.AskID, elapsed)
		}
	}
	return nil
}

func (d *oracleAttentionDriver) tracef(format string, args ...any) {
	if d.out != nil {
		fmt.Fprintf(d.out, format+"\n", args...)
	}
}
