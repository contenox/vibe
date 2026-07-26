package hitlservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
)

// The two fields that mark a durable ask as an ATTENTION ask rather than a
// permission one. They are the tool identity the asking unit actually called, so
// the discriminator is a fact about the row rather than a flag invented for it —
// which is what lets every existing surface (the approvals API, `contenox
// approvals`, beam's inbox) list both kinds without knowing there are two.
const (
	AttentionToolsName = "mission"
	AttentionToolName  = "mission_ask_attention"
)

// ErrAttentionUnanswered reports that an attention ask reached its deadline with
// nobody answering, or was answered with a refusal. It is the signal the caller
// turns into its own fallback — for a mission unit, filing the question as a
// durable blocker report so it is not lost — never a crash.
var ErrAttentionUnanswered = errors.New("hitlservice: attention ask went unanswered")

// AttentionRequest is one unit's question for a human. It is deliberately much
// narrower than ApprovalRequest: there is no policy verdict to carry (an
// attention ask is not gated by a rule — the unit decided it needs a person),
// no diff, and no args. What it has is a question and the attribution that says
// whose question it is.
type AttentionRequest struct {
	// Summary is the one-line question. Required.
	Summary string
	// Detail is the optional longer form — context the operator needs to answer.
	Detail string
	// MissionID, InstanceID, SessionID and AgentName attribute the ask, exactly
	// as they do on a permission ask, so an inbox can say which unit is waiting
	// and an operator can reach it.
	MissionID  string
	InstanceID string
	SessionID  string
	AgentName  string

	// OnRaised, when set, is called with the durable ask's id the moment the row
	// exists and BEFORE the wait begins. It is the seam a caller uses to announce
	// the question somewhere else — publishing it onto the bus so the session that
	// fired the mission learns about it — which cannot be done from the return
	// value, since that only arrives once the question has been answered.
	//
	// It runs inline: keep it cheap and non-blocking (a publish), because the unit
	// is not yet parked while it runs. Panicking is the caller's own bug; errors
	// are the caller's to swallow, since a question that was recorded but not
	// announced is still answerable from the ask queue.
	OnRaised func(askID string)
}

// IsAttentionAsk reports whether a durable ask row is an attention ask — a
// question expecting DATA — rather than a permission ask expecting yes/no. Every
// surface that renders or answers asks branches on this: answering an attention
// ask with a bare approve leaves the asking unit with no answer at all.
func IsAttentionAsk(row *runtimetypes.HITLApproval) bool {
	return row != nil && row.ToolsName == AttentionToolsName && row.ToolName == AttentionToolName
}

// AnswerOf returns the operator's text answer from a resolved attention ask, or
// "" when the row carries none (still pending, expired, or a permission ask).
func AnswerOf(row *runtimetypes.HITLApproval) string {
	if row == nil || len(row.Resolution) == 0 {
		return ""
	}
	var res approvalResolution
	if err := json.Unmarshal(row.Resolution, &res); err != nil || res.Answer == nil {
		return ""
	}
	return *res.Answer
}

// RequestAttention records a unit's question as a durable ask and BLOCKS until
// an operator answers it, the serve-level ceiling expires it, or ctx ends —
// returning the operator's own words.
//
// It is the other half of a promise the runtime already made to the model: the
// mission tool is described to it as "ask a question, or flag a blocker you must
// not decide alone", and until this existed every such question was silently
// downgraded to a blocker report that no surface could answer. A unit could ask;
// nobody could reply.
//
// It deliberately reuses the permission ask's machinery — same durable row, same
// pending-waiter map, same expiry sweep — because an attention ask differs from a
// permission ask in exactly one way that matters here: its answer is text. Two
// stores for two kinds of "a human owes this unit something" would be the second
// mechanism this codebase keeps refusing to grow.
func (s *service) RequestAttention(ctx context.Context, req AttentionRequest, sink taskengine.TaskEventSink) (string, error) {
	if s.approvals == nil {
		return "", fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		return "", fmt.Errorf("hitlservice: attention ask requires a summary")
	}

	askID := uuid.NewString()
	now := time.Now().UTC()
	// No matched rule sets a deadline for an attention ask (no rule produced it),
	// so it is bounded by the serve-level ceiling like any unbounded permission
	// ask — an operator who never answers must not park a unit forever.
	timeout := s.ceiling()

	row := &runtimetypes.HITLApproval{
		ID:          askID,
		ToolsName:   AttentionToolsName,
		ToolName:    AttentionToolName,
		ArgsSummary: summary,
		// OnTimeout is "deny" in the shared vocabulary: nobody answered. The
		// caller's fallback (a blocker report) is what actually preserves the
		// question, so this only decides how the row comes to rest.
		OnTimeout:  string(ActionDeny),
		State:      runtimetypes.HITLApprovalPending,
		InstanceID: req.InstanceID,
		SessionID:  req.SessionID,
		AgentName:  req.AgentName,
		CreatedAt:  now,
		ExpiresAt:  now.Add(timeout),
	}
	if detail := strings.TrimSpace(req.Detail); detail != "" {
		// The long form rides the Diff column — the row's one free-text field,
		// which the inbox already renders beneath the summary.
		row.Diff = &detail
	}
	if req.MissionID != "" {
		missionID := req.MissionID
		row.MissionID = &missionID
	}
	// Durable FIRST, exactly as RequestApproval does: a restart between here and
	// the answer must still show the question pending rather than lose it.
	if err := s.approvals.CreateHITLApproval(ctx, row); err != nil {
		return "", fmt.Errorf("hitlservice: persist attention ask: %w", err)
	}

	if req.OnRaised != nil {
		req.OnRaised(askID)
	}

	ch := make(chan answer, 1)
	s.mu.Lock()
	s.pending[askID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, askID)
		s.mu.Unlock()
	}()

	// The same event a permission ask publishes, so a live client's approval
	// surface shows the question the moment it is raised rather than on its next
	// poll. Best-effort: an unpublished event costs immediacy, not the ask.
	if sink != nil {
		ev := taskengine.NewTaskEvent(ctx, taskengine.TaskEventApprovalRequested)
		ev.ApprovalID = askID
		ev.HookName = AttentionToolsName
		ev.ToolName = AttentionToolName
		ev.ApprovalArgs = map[string]any{"summary": summary, "detail": req.Detail}
		_ = sink.PublishTaskEvent(ctx, ev)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The wait watches the DURABLE ROW as well as the in-process channel, and
	// that is load-bearing rather than belt-and-braces: an attention ask is
	// raised by the dispatched UNIT's own process, while the operator answers it
	// in the process that owns the API — `contenox serve`. Two processes, one
	// shared SQLite state, and a Go channel that cannot cross between them. The
	// unit already reaches its operator this way for reports (missionservice
	// writes to the same shared store), so the answer comes back the same way.
	//
	// The channel is still first: when raiser and answerer ARE the same process
	// (an in-process editor firing its own mission), the wake-up is immediate and
	// the poll never fires.
	poll := time.NewTicker(attentionPollInterval)
	defer poll.Stop()

	for {
		select {
		case ans := <-ch:
			if !ans.approved || strings.TrimSpace(ans.text) == "" {
				// Answered with a refusal, or with nothing to say: the unit gets no
				// guidance, which its fallback must treat as "still blocked".
				return "", ErrAttentionUnanswered
			}
			return ans.text, nil
		case <-poll.C:
			row, err := s.approvals.GetHITLApproval(ctx, askID)
			if err != nil || row.State == runtimetypes.HITLApprovalPending {
				continue // unreadable right now, or still waiting on a human
			}
			if text := AnswerOf(row); strings.TrimSpace(text) != "" {
				return text, nil
			}
			// Terminal without an answer: denied, expired, or resolved by the
			// boolean path. Nothing to act on.
			return "", ErrAttentionUnanswered
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// The ceiling fired. The row is left pending; SweepExpired closes it out.
			return "", ErrAttentionUnanswered
		}
	}
}

// attentionPollInterval is how often a parked unit re-reads its own ask row for
// an answer written by another process. A human takes seconds at best, so this
// is a cheap single-row read on a cadence nobody notices — deliberately not
// tuned down to feel "instant", because the same-process case already is (the
// channel).
const attentionPollInterval = time.Second

// Answer resolves an ATTENTION ask with the operator's text, waking the unit
// parked on it. It is the text-carrying sibling of Respond, and it refuses a
// permission ask by design: answering "write_file?" with prose would resolve the
// gate with no verdict at all.
func (s *service) Answer(ctx context.Context, askID, text string) error {
	return s.answerAttention(ctx, askID, text, "")
}

// answerAttention is the shared body of Answer (a human) and AnswerAsAgent (a
// supervising model): identical resolution and wake-up, differing only in the
// actor recorded on the durable row.
func (s *service) answerAttention(ctx context.Context, askID, text, by string) error {
	if s.approvals == nil {
		return fmt.Errorf("hitlservice: durable approval store not configured; pass a runtimetypes.Store-backed store to New/NewWithDefaultPolicy")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("hitlservice: an attention answer cannot be empty")
	}
	row, err := s.approvals.GetHITLApproval(ctx, askID)
	if err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return ErrApprovalNotFound
		}
		return fmt.Errorf("hitlservice: look up ask %s: %w", askID, err)
	}
	if !IsAttentionAsk(row) {
		return fmt.Errorf("hitlservice: ask %s is a permission request (%s.%s), which is answered approve/deny, not with text",
			askID, row.ToolsName, row.ToolName)
	}

	now := time.Now().UTC()
	if err := s.approvals.ResolveHITLApproval(ctx, askID, runtimetypes.HITLApprovalApproved, marshalAttentionResolution(text, by), now); err != nil {
		if !errors.Is(err, libdb.ErrNotFound) {
			return fmt.Errorf("hitlservice: resolve ask %s: %w", askID, err)
		}
		// Lost the compare-and-swap: tell expired from already-answered, the same
		// way Respond does.
		current, getErr := s.approvals.GetHITLApproval(ctx, askID)
		if getErr != nil {
			return fmt.Errorf("hitlservice: look up ask %s: %w", askID, getErr)
		}
		if current.State == runtimetypes.HITLApprovalExpired {
			return ErrApprovalExpired
		}
		return ErrApprovalAlreadyResolved
	}

	s.mu.Lock()
	ch, ok := s.pending[askID]
	s.mu.Unlock()
	if ok {
		select {
		case ch <- answer{approved: true, text: text}:
		default:
		}
	}
	return nil
}

// answeredByAgent marks a resolution written by a supervising AGENT rather than
// by a human. It is what the envelope's agent-answer cap is counted on, and what
// an audit reads to tell "a person decided this" from "another model did".
const answeredByAgent = "agent"

// AnswerAsAgent resolves an attention ask exactly as Answer does, but records
// that an AGENT answered it — the supervising session's model replying to its own
// subagent rather than a human replying to either.
//
// The distinction is not cosmetic. A unit escalates to a human on purpose; letting
// a model answer in the human's place is a governance choice the mission's
// envelope makes (see AgentAnswerCount, and the caller that consults it), and a
// bound nobody can count is not a bound. Recording the actor on the durable row is
// what makes the count possible after a restart, and what lets an operator see,
// later, that no human ever looked at this question.
func (s *service) AnswerAsAgent(ctx context.Context, askID, text string) error {
	return s.answerAttention(ctx, askID, text, answeredByAgent)
}

// PendingAttentionAsks returns missionID's unanswered questions, newest first —
// what that mission is currently blocked on.
func (s *service) PendingAttentionAsks(ctx context.Context, missionID string) ([]*runtimetypes.HITLApproval, error) {
	if s.approvals == nil {
		return nil, fmt.Errorf("hitlservice: durable approval store not configured")
	}
	rows, err := s.approvals.ListHITLApprovalsForMission(ctx, missionID, missionAskScanLimit)
	if err != nil {
		return nil, fmt.Errorf("hitlservice: list asks for mission %s: %w", missionID, err)
	}
	out := make([]*runtimetypes.HITLApproval, 0, len(rows))
	for _, row := range rows {
		if IsAttentionAsk(row) && row.State == runtimetypes.HITLApprovalPending {
			out = append(out, row)
		}
	}
	return out, nil
}

// AgentAnswerCount reports how many of missionID's questions were answered by a
// supervising AGENT rather than a human. It is the durable counter an envelope's
// cap is enforced against: a runaway question loop between a unit and its
// supervisor is bounded by a number that survives a restart, not by an in-memory
// tally that a redeploy would reset to zero.
func (s *service) AgentAnswerCount(ctx context.Context, missionID string) (int, error) {
	if s.approvals == nil {
		return 0, fmt.Errorf("hitlservice: durable approval store not configured")
	}
	rows, err := s.approvals.ListHITLApprovalsForMission(ctx, missionID, missionAskScanLimit)
	if err != nil {
		return 0, fmt.Errorf("hitlservice: list asks for mission %s: %w", missionID, err)
	}
	count := 0
	for _, row := range rows {
		if !IsAttentionAsk(row) || len(row.Resolution) == 0 {
			continue
		}
		var res approvalResolution
		if json.Unmarshal(row.Resolution, &res) != nil {
			continue
		}
		if res.AnsweredBy != nil && *res.AnsweredBy == answeredByAgent {
			count++
		}
	}
	return count, nil
}

// missionAskScanLimit bounds the per-mission ask scan. A mission that has asked
// more than this many questions has a bigger problem than an off-by-a-few count.
const missionAskScanLimit = 200
