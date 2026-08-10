package acpsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
)

// AskInbox is the slice of hitlservice.Service the session-attached ask paths
// need: read what is unanswered, and record one operator verdict. The host
// must wire the process's OWN hitlservice instance (the one the engine gates
// through) — a second instance cannot wake a waiter parked in the first, and
// only the wired one carries the resume hook a checkpointed run needs.
//
// Two consumers share it, and deliberately share one Deps field rather than
// two: /answer (commands_answer.go) and the attach-time re-offer of a parked
// approval (reoffer.go). Both are the same capability — this process can
// close a durable ask and resume what is parked on it — so splitting them
// into separate wiring points would let a host turn one on and leave the
// other silently dark.
type AskInbox interface {
	// PendingAttentionAsks returns a mission's unanswered questions, newest first.
	PendingAttentionAsks(ctx context.Context, missionID string) ([]*runtimetypes.HITLApproval, error)
	// Answer resolves a pending question with the operator's reply and wakes
	// the unit parked on it; it rejects a permission ask.
	Answer(ctx context.Context, askID, text string) error
	// ListPendingForSession returns one session's unanswered asks, newest
	// first — the rows a client attaching to that session must be shown
	// again. An empty session id matches nothing.
	ListPendingForSession(ctx context.Context, sessionID string, limit int) ([]*runtimetypes.HITLApproval, error)
	// Respond records a permission verdict on a durable ask, waking a parked
	// waiter or, when none is left, running the resume hook against the
	// checkpointed run.
	//
	// This is the only path a re-offered card may resolve through. The
	// goroutine that raised the original ask was abandoned when
	// localtools.ApprovalParkWindow elapsed, so a verdict that stops at this
	// surface unblocks nothing: it would tell the operator they released a
	// run that is still parked.
	Respond(ctx context.Context, approvalID string, approved bool) error
}

// MissionSupervision resolves which missions a session fired — the ownership
// check /answer applies before it answers anything. It is the same seam
// missiontools.SupervisorStore reads through, so the slash command and the
// agent's own mission_answer tool agree on what "yours" means.
type MissionSupervision interface {
	// MissionsFiredBy returns the missions whose ParentSessionID is
	// parentSessionID, newest first.
	MissionsFiredBy(ctx context.Context, parentSessionID string, limit int) ([]*missionservice.Mission, error)
}

// answerMissionScanLimit bounds how many of a session's missions one /answer
// consults, mirroring missiontools' supervisorMissionLimit.
const answerMissionScanLimit = 20

// answerUsageLine is the one-line grammar every /answer refusal repeats, so
// the shape is learned from whichever error comes first.
const answerUsageLine = "usage: /answer <ask-id> <your answer>"

// hasAnswerCapability reports whether /answer can run: the durable ask inbox
// and the mission-ownership seam, both wired only by a host that embeds the
// fleet in-process. Gates whether /answer is advertised at all — never
// advertise a command that can only error out.
func (t *Transport) hasAnswerCapability() bool {
	return t.deps.Asks != nil && t.deps.Supervision != nil
}

// answerableAsk pairs one pending question with the mission it came from.
type answerableAsk struct {
	row     *runtimetypes.HITLApproval
	mission *missionservice.Mission
}

// handleAnswer answers a question one of this session's mission units is
// parked on (`/answer <ask-id> <text>`), the in-session equivalent of
// `contenox approvals respond <id> --answer`. It records the answer through
// the same hitlservice.Answer that command uses, so a run checkpointed under
// the ask resumes through the process's resume hook exactly as it would from
// a second terminal. With no argument it lists what is waiting.
//
// The answer is recorded as a HUMAN answer, so the mission envelope's
// agent-answer bound (hitlservice.EnforceAgentAnswerBounds) is not consulted
// — that bound counts answers ATTRIBUTED TO AN AGENT, and an operator typing
// a slash command is not one. This is the same split `approvals respond
// --answer` makes against its `--as-agent` sibling, which is the only path
// that spends the bound. Ownership is checked instead: the ask must belong to
// a mission this session fired, since an answer is an instruction the unit
// acts on immediately, not a read.
func (t *Transport) handleAnswer(ctx context.Context, sess *sessionEntry, args string) (string, error) {
	if !t.hasAnswerCapability() {
		return "", fmt.Errorf("answering questions is unavailable in this session: it needs the in-process fleet. Answer from a terminal instead: `contenox approvals list`, then `contenox approvals respond <ask-id> --answer \"...\"`")
	}
	parentSessionID := sess.InternalSessionID
	if parentSessionID == "" {
		return "", fmt.Errorf("this session has no durable record, so no mission was fired from it")
	}

	askID, text := splitFirstToken(args)
	if askID == "" {
		return t.answerableAsksListing(ctx, parentSessionID)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s — /answer alone lists the questions waiting on you", answerUsageLine)
	}

	found, err := t.findAnswerableAsk(ctx, parentSessionID, askID)
	if err != nil {
		return "", err
	}
	if found == nil {
		return "", t.unanswerableAskError(ctx, askID)
	}
	if err := t.deps.Asks.Answer(ctx, askID, text); err != nil {
		return "", answerFailure(askID, found.row, err)
	}

	unit := strings.TrimSpace(found.mission.AgentName)
	if unit == "" {
		unit = "the unit"
	} else {
		unit = fmt.Sprintf("unit %q", unit)
	}
	return fmt.Sprintf("Answered %s — %s has your reply and continues. Further questions from it arrive here.", askID, unit), nil
}

// answerableAsks collects the pending questions of every mission this session
// fired. A mission whose asks cannot be read is skipped rather than failing
// the whole listing: one unreadable mission must not hide the others.
func (t *Transport) answerableAsks(ctx context.Context, parentSessionID string) ([]answerableAsk, error) {
	missions, err := t.deps.Supervision.MissionsFiredBy(ctx, parentSessionID, answerMissionScanLimit)
	if err != nil {
		return nil, fmt.Errorf("could not read this session's missions: %w", err)
	}
	var out []answerableAsk
	for _, m := range missions {
		if m == nil {
			continue
		}
		rows, err := t.deps.Asks.PendingAttentionAsks(ctx, m.ID)
		if err != nil {
			continue
		}
		for _, row := range rows {
			out = append(out, answerableAsk{row: row, mission: m})
		}
	}
	return out, nil
}

// findAnswerableAsk resolves askID against this session's own pending
// questions. A nil result is "not yours, or not pending" — never an error, so
// the caller can answer with the one teaching line unanswerableAskError builds.
func (t *Transport) findAnswerableAsk(ctx context.Context, parentSessionID, askID string) (*answerableAsk, error) {
	asks, err := t.answerableAsks(ctx, parentSessionID)
	if err != nil {
		return nil, err
	}
	for i := range asks {
		if asks[i].row != nil && asks[i].row.ID == askID {
			return &asks[i], nil
		}
	}
	return nil, nil
}

// answerableAsksListing is what `/answer` alone reports: the questions this
// session's units are parked on, with the handle each is answered by.
func (t *Transport) answerableAsksListing(ctx context.Context, parentSessionID string) (string, error) {
	asks, err := t.answerableAsks(ctx, parentSessionID)
	if err != nil {
		return "", err
	}
	if len(asks) == 0 {
		return "Nothing is waiting on you. A unit's question arrives in this session as it is asked; /mission fires one.", nil
	}
	now := time.Now().UTC()
	var b strings.Builder
	b.WriteString("Questions waiting on you:\n")
	for _, a := range asks {
		unit := strings.TrimSpace(a.mission.AgentName)
		if unit == "" {
			unit = "unit"
		}
		fmt.Fprintf(&b, "  %s  %s — %s", a.row.ID, unit, strings.TrimSpace(a.row.ArgsSummary))
		if window := untilLabel(now, a.row.ExpiresAt); window != "" {
			fmt.Fprintf(&b, "  (answerable for %s)", window)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nAnswer one with: " + answerUsageLine)
	return strings.TrimRight(b.String(), "\n"), nil
}

// unanswerableAskError explains an id that is not among this session's
// pending questions, reading the durable row to say which of the four
// possible reasons applies — including the kind mismatch `approvals respond`
// teaches (a permission ask takes a verdict, never text).
func (t *Transport) unanswerableAskError(ctx context.Context, askID string) error {
	if t.deps.DB == nil {
		return fmt.Errorf("no question %q is waiting on you — /answer lists what is", askID)
	}
	row, err := runtimetypes.New(t.deps.DB.WithoutTransaction()).GetHITLApproval(ctx, askID)
	if err != nil {
		return fmt.Errorf("no ask %q exists — /answer lists the questions waiting on you", askID)
	}
	if !hitlservice.IsAttentionAsk(row) {
		return fmt.Errorf("ask %s is a permission request (%s.%s), not a question — it takes approve or deny, not text. Answer it on its permission card, or run `contenox approvals respond %s --approve|--deny`",
			askID, row.ToolsName, row.ToolName, askID)
	}
	if row.State != runtimetypes.HITLApprovalPending {
		return fmt.Errorf("question %s was already answered — a verdict is recorded exactly once", askID)
	}
	return fmt.Errorf("question %s belongs to a mission this session did not fire — answer it from the session that did, or with `contenox approvals respond %s --answer \"...\"`", askID, askID)
}

// answerFailure phrases a refused or failed answer for the operator, mapping
// hitlservice's sentinels (which name the package and the internal state) onto
// what actually happened and what to do next. Mirrors the branch table
// `contenox approvals respond` applies to the same errors.
func answerFailure(askID string, row *runtimetypes.HITLApproval, err error) error {
	switch {
	case errors.Is(err, hitlservice.ErrApprovalNotFound):
		return fmt.Errorf("question %s no longer exists — it was resolved while you were typing", askID)
	case errors.Is(err, hitlservice.ErrApprovalAlreadyResolved):
		return fmt.Errorf("question %s was already answered — a verdict is recorded exactly once", askID)
	case errors.Is(err, hitlservice.ErrApprovalExpired):
		onTimeout := "its on-timeout verdict"
		if row != nil && row.OnTimeout != "" {
			onTimeout = fmt.Sprintf("its on-timeout verdict (%s)", row.OnTimeout)
		}
		return fmt.Errorf("question %s expired before this answer; %s already applied", askID, onTimeout)
	case errors.Is(err, hitlservice.ErrVerdictNeedsResumer):
		return fmt.Errorf("question %s has a suspended run checkpointed under it, and this process cannot resume it. The answer was NOT recorded and the question is still waiting — answer it from a terminal that can reach your models: `contenox approvals respond %s --answer \"...\"`", askID, askID)
	case errors.Is(err, libdb.ErrNotFound):
		return fmt.Errorf("question %s no longer exists — it was resolved while you were typing", askID)
	}
	return fmt.Errorf("could not answer %s: %w", askID, err)
}

// untilLabel renders how much longer a deadline is away, in the coarse units
// a countdown is read in. Empty for an unset or already-passed deadline, so a
// caller omits the clause rather than promising a window that is gone.
func untilLabel(now, deadline time.Time) string {
	if deadline.IsZero() {
		return ""
	}
	d := deadline.Sub(now)
	if d <= 0 {
		return ""
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
