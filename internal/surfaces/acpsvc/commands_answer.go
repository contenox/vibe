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
// need. The host must wire the process's own hitlservice instance: a second
// instance cannot wake a waiter parked in the first, and only the wired one
// carries the resume hook a checkpointed run needs.
type AskInbox interface {
	PendingAttentionAsks(ctx context.Context, missionID string) ([]*runtimetypes.HITLApproval, error)
	Answer(ctx context.Context, askID, text string) error
	ListPendingForSession(ctx context.Context, sessionID string, limit int) ([]*runtimetypes.HITLApproval, error)
	Respond(ctx context.Context, approvalID string, approved bool) error
}

// MissionSupervision resolves which missions a session fired, the ownership
// check /answer applies before it answers anything.
type MissionSupervision interface {
	MissionsFiredBy(ctx context.Context, parentSessionID string, limit int) ([]*missionservice.Mission, error)
}

const answerMissionScanLimit = 20

const answerUsageLine = "usage: /answer <ask-id> <your answer>"

func (t *Transport) hasAnswerCapability() bool {
	return t.deps.Asks != nil && t.deps.Supervision != nil
}

type answerableAsk struct {
	row     *runtimetypes.HITLApproval
	mission *missionservice.Mission
}

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

// findAnswerableAsk resolves askID against this session's own pending questions;
// a nil result means "not yours, or not pending" and is not an error.
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
		if a.row.ExpiresAt.IsZero() {
			b.WriteString("  (no deadline — answerable until you answer it)")
		} else if window := untilLabel(now, a.row.ExpiresAt); window != "" {
			fmt.Fprintf(&b, "  (answerable for %s)", window)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nAnswer one with: " + answerUsageLine)
	return strings.TrimRight(b.String(), "\n"), nil
}

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

// untilLabel renders how much longer a deadline is away; empty for an unset or
// already-passed deadline.
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
