package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relaylink"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

type capturedFrames struct {
	mu     sync.Mutex
	frames []librelay.Frame
	err    error
}

func (c *capturedFrames) send(f librelay.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.frames = append(c.frames, f)
	return nil
}

func (c *capturedFrames) all() []librelay.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]librelay.Frame(nil), c.frames...)
}

type recordedVerdict struct {
	askID     string
	approved  bool
	answer    string
	kind      string
	decidedBy string
	guidance  string
}

type fakeAskInbox struct {
	err     error
	pending []*runtimetypes.HITLApproval

	mu   sync.Mutex
	seen []recordedVerdict
	done chan struct{}
}

func (i *fakeAskInbox) AnswerFrom(_ context.Context, askID, text, by string) error {
	return i.record(recordedVerdict{askID: askID, answer: text, kind: "answer", decidedBy: by})
}

func (i *fakeAskInbox) RespondWithGuidance(_ context.Context, askID string, approved bool, decidedBy, guidance string) error {
	return i.record(recordedVerdict{
		askID: askID, approved: approved, kind: "respond",
		decidedBy: decidedBy, guidance: guidance,
	})
}

func (i *fakeAskInbox) ListPending(_ context.Context, limit int) ([]*runtimetypes.HITLApproval, error) {
	if i.err != nil {
		return nil, i.err
	}
	if limit > 0 && len(i.pending) > limit {
		return i.pending[:limit], nil
	}
	return i.pending, nil
}

func (i *fakeAskInbox) record(v recordedVerdict) error {
	i.mu.Lock()
	i.seen = append(i.seen, v)
	done := i.done
	i.mu.Unlock()
	if done != nil {
		close(done)
		i.mu.Lock()
		i.done = nil
		i.mu.Unlock()
	}
	return i.err
}

func (i *fakeAskInbox) all() []recordedVerdict {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]recordedVerdict(nil), i.seen...)
}

func (i *fakeAskInbox) expect() <-chan struct{} {
	ch := make(chan struct{})
	i.mu.Lock()
	i.done = ch
	i.mu.Unlock()
	return ch
}

var _ askInbox = (*fakeAskInbox)(nil)

func attachedBridge(inbox askInbox) (*relayAskBridge, *capturedFrames) {
	sent := &capturedFrames{}
	b := newRelayAskBridge(inbox, libtracker.NoopTracker{})
	b.attach("inst-1", sent.send)
	return b, sent
}

func pendingAskRow() *runtimetypes.HITLApproval {
	rule := 3
	mission := "m-1"
	diff := "--- secret\n+++ secret"
	return &runtimetypes.HITLApproval{
		ID:          "ask-1",
		ToolsName:   "billing",
		ToolName:    "issue_refund",
		ArgsSummary: "refund 40 EUR to customer 8812",
		Diff:        &diff,
		PolicyName:  "hitl-policy-default.json",
		MatchedRule: &rule,
		OnTimeout:   "deny",
		State:       runtimetypes.HITLApprovalPending,
		SessionID:   "cnx-sess-1",
		AgentName:   "refund-desk",
		MissionID:   &mission,
		CreatedAt:   time.Date(2026, 8, 16, 11, 0, 0, 0, time.UTC),
		ExpiresAt:   time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
}

func TestUnit_RelayAskBridge_PublishesAnAskAndItsRetraction(t *testing.T) {
	t.Parallel()
	b, sent := attachedBridge(nil)

	b.AskRecorded(t.Context(), pendingAskRow())
	b.AskResolved(t.Context(), "ask-1", hitlservice.AskAnswered)

	frames := sent.all()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2: %+v", len(frames), frames)
	}
	if frames[0].Type != librelay.TypeAskPublished || frames[0].Instance != "inst-1" || frames[0].Session != "" {
		t.Fatalf("published envelope = %+v", frames[0])
	}
	var published librelay.AskPublished
	if err := frames[0].DecodePayload(&published); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if published.MatchedRule == nil || *published.MatchedRule != 3 {
		t.Fatalf("matched rule = %v", published.MatchedRule)
	}
	want := librelay.AskPublished{
		AskID:       "ask-1",
		SessionID:   "cnx-sess-1",
		MissionID:   "m-1",
		AgentName:   "refund-desk",
		ToolsName:   "billing",
		ToolName:    "issue_refund",
		PolicyName:  "hitl-policy-default.json",
		ArgsSummary: "refund 40 EUR to customer 8812",
		ExpiresAt:   time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
	published.MatchedRule = nil
	if published != want {
		t.Fatalf("published = %+v, want %+v", published, want)
	}

	if frames[1].Type != librelay.TypeAskResolved {
		t.Fatalf("resolved envelope = %+v", frames[1])
	}
	var resolved librelay.AskResolved
	if err := frames[1].DecodePayload(&resolved); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if resolved != (librelay.AskResolved{AskID: "ask-1", Reason: librelay.AskResolvedAnswered}) {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestUnit_RelayAskBridge_PublishedAskCarriesNoGatedCallInput(t *testing.T) {
	t.Parallel()
	b, sent := attachedBridge(nil)
	b.AskRecorded(t.Context(), pendingAskRow())

	frames := sent.all()
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(frames[0].Payload, &fields); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, forbidden := range []string{"diff", "args", "arguments", "input", "resolution", "on_timeout"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("payload carries %q: %s", forbidden, frames[0].Payload)
		}
	}
}

func TestUnit_RelayAskBridge_NamesWhyAnAskWasRetracted(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reason hitlservice.AskResolution
		want   string
	}{
		"answered":   {reason: hitlservice.AskAnswered, want: librelay.AskResolvedAnswered},
		"expired":    {reason: hitlservice.AskExpired, want: librelay.AskResolvedExpired},
		"superseded": {reason: hitlservice.AskSuperseded, want: librelay.AskResolvedSuperseded},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, sent := attachedBridge(nil)
			b.AskResolved(t.Context(), "ask-1", tc.reason)
			frames := sent.all()
			if len(frames) != 1 {
				t.Fatalf("frames = %d, want 1", len(frames))
			}
			var resolved librelay.AskResolved
			if err := frames[0].DecodePayload(&resolved); err != nil {
				t.Fatalf("DecodePayload: %v", err)
			}
			if resolved.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", resolved.Reason, tc.want)
			}
		})
	}
}

func TestUnit_RelayAskBridge_WithNoConnectionSaysNothingAndFailsNothing(t *testing.T) {
	t.Parallel()
	for name, b := range map[string]*relayAskBridge{
		"never attached":       newRelayAskBridge(nil, libtracker.NoopTracker{}),
		"not built at all":     nil,
		"built without inputs": newRelayAskBridge(nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			b.AskRecorded(t.Context(), pendingAskRow())
			b.AskResolved(t.Context(), "ask-1", hitlservice.AskAnswered)
			b.handleVerdict(t.Context(), librelay.Frame{Type: librelay.TypeAskVerdict})
			b.detach()
		})
	}
}

func TestUnit_RelayAskBridge_ADroppedLinkNeverFailsThePublisher(t *testing.T) {
	t.Parallel()
	for name, sendErr := range map[string]error{
		"the link is down":        relaylink.ErrNotConnected,
		"the queue is full":       relaylink.ErrBacklogFull,
		"the connector is closed": relaylink.ErrClosed,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sent := &capturedFrames{err: sendErr}
			b := newRelayAskBridge(nil, libtracker.NoopTracker{})
			b.attach("inst-1", sent.send)
			b.AskRecorded(t.Context(), pendingAskRow())
			b.AskResolved(t.Context(), "ask-1", hitlservice.AskAnswered)
		})
	}
}

func TestUnit_RelayAskBridge_DetachStopsPublishing(t *testing.T) {
	t.Parallel()
	b, sent := attachedBridge(nil)
	b.detach()
	b.AskRecorded(t.Context(), pendingAskRow())
	if frames := sent.all(); len(frames) != 0 {
		t.Fatalf("a detached bridge sent %d frames", len(frames))
	}
}

func TestUnit_RelayAskBridge_SettlesAVerdictThroughTheInboxAnAttachedClientUses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		verdict librelay.AskVerdict
		want    recordedVerdict
	}{
		"allow": {
			verdict: librelay.AskVerdict{AskID: "ask-1", Decision: librelay.AskDecisionAllow, DecidedBy: "u_9"},
			want:    recordedVerdict{askID: "ask-1", approved: true, kind: "respond", decidedBy: "u_9"},
		},
		"deny": {
			verdict: librelay.AskVerdict{AskID: "ask-1", Decision: librelay.AskDecisionDeny, DecidedBy: "u_9", Guidance: "outside the window"},
			want:    recordedVerdict{askID: "ask-1", kind: "respond", decidedBy: "u_9", guidance: "outside the window"},
		},
		"answer": {
			verdict: librelay.AskVerdict{AskID: "ask-1", Decision: librelay.AskDecisionAnswer, Answer: "use the 2019 table", DecidedBy: "u_9"},
			want:    recordedVerdict{askID: "ask-1", answer: "use the 2019 table", kind: "answer", decidedBy: "u_9"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inbox := &fakeAskInbox{}
			b, _ := attachedBridge(inbox)
			settled := inbox.expect()

			f, err := librelay.Frame{Type: librelay.TypeAskVerdict, Instance: "inst-1"}.WithPayload(tc.verdict)
			if err != nil {
				t.Fatalf("WithPayload: %v", err)
			}
			b.handleVerdict(t.Context(), f)
			select {
			case <-settled:
			case <-time.After(5 * time.Second):
				t.Fatal("the verdict never reached the inbox")
			}
			b.detach()

			seen := inbox.all()
			if len(seen) != 1 || seen[0] != tc.want {
				t.Fatalf("inbox saw %+v, want exactly one %+v", seen, tc.want)
			}
		})
	}
}

func TestUnit_RelayAskBridge_RefusesAVerdictItCannotSettle(t *testing.T) {
	t.Parallel()
	for name, payload := range map[string]any{
		"no ask id":                librelay.AskVerdict{Decision: librelay.AskDecisionAllow},
		"an unknown decision":      librelay.AskVerdict{AskID: "ask-1", Decision: "escalate"},
		"no decision at all":       librelay.AskVerdict{AskID: "ask-1"},
		"an answer with no answer": librelay.AskVerdict{AskID: "ask-1", Decision: librelay.AskDecisionAnswer},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inbox := &fakeAskInbox{}
			b, sent := attachedBridge(inbox)

			f, err := librelay.Frame{Type: librelay.TypeAskVerdict, Instance: "inst-1"}.WithPayload(payload)
			if err != nil {
				t.Fatalf("WithPayload: %v", err)
			}
			b.handleVerdict(t.Context(), f)
			b.detach()

			if seen := inbox.all(); len(seen) != 0 {
				t.Fatalf("an unusable verdict reached the inbox: %+v", seen)
			}
			if frames := sent.all(); len(frames) != 0 {
				t.Fatalf("a notification was answered with %+v", frames)
			}
		})
	}
}

func TestUnit_RelayAskBridge_IgnoresAVerdictAddressedToAnotherMachine(t *testing.T) {
	t.Parallel()
	inbox := &fakeAskInbox{}
	b, _ := attachedBridge(inbox)

	f, err := librelay.Frame{Type: librelay.TypeAskVerdict, Instance: "inst-2"}.
		WithPayload(librelay.AskVerdict{AskID: "ask-1", Decision: librelay.AskDecisionAllow})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	b.handleVerdict(t.Context(), f)
	b.detach()

	if seen := inbox.all(); len(seen) != 0 {
		t.Fatalf("a verdict for another instance was applied: %+v", seen)
	}
}

func TestUnit_RelayAskBridge_AMalformedRequestIsAnswered(t *testing.T) {
	t.Parallel()
	inbox := &fakeAskInbox{}
	b, sent := attachedBridge(inbox)

	b.handleVerdict(t.Context(), librelay.Frame{
		Type:     librelay.TypeAskVerdict,
		Instance: "inst-1",
		ID:       "v-1",
		Payload:  json.RawMessage(`{"ask_id":5}`),
	})
	b.detach()

	frames := sent.all()
	if len(frames) != 1 || frames[0].Type != librelay.TypeError || frames[0].ReplyTo != "v-1" {
		t.Fatalf("frames = %+v", frames)
	}
	var e librelay.Error
	if err := frames[0].DecodePayload(&e); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if e.Code != librelay.CodeMalformedFrame {
		t.Fatalf("code = %q", e.Code)
	}
	if seen := inbox.all(); len(seen) != 0 {
		t.Fatalf("a malformed verdict reached the inbox: %+v", seen)
	}
}

func TestUnit_RelayAskBridge_AnAlreadySettledAskIsNotAnError(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"already resolved": hitlservice.ErrApprovalAlreadyResolved,
		"never existed":    hitlservice.ErrApprovalNotFound,
		"expired first":    hitlservice.ErrApprovalExpired,
		"unresumable":      hitlservice.ErrVerdictNeedsResumer,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inbox := &fakeAskInbox{err: err}
			b, _ := attachedBridge(inbox)
			settled := inbox.expect()

			f, ferr := librelay.Frame{Type: librelay.TypeAskVerdict, Instance: "inst-1"}.
				WithPayload(librelay.AskVerdict{AskID: "ask-1", Decision: librelay.AskDecisionDeny})
			if ferr != nil {
				t.Fatalf("WithPayload: %v", ferr)
			}
			b.handleVerdict(t.Context(), f)
			select {
			case <-settled:
			case <-time.After(5 * time.Second):
				t.Fatal("the verdict never reached the inbox")
			}
			b.detach()
		})
	}
}

func TestUnit_RelayAskBridge_ARelayedVerdictWalksTheSamePathAsALocalOne(t *testing.T) {
	ctx := t.Context()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "relay-asks.db"), runtimetypes.SchemaSQLite)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{}, "")

	bridge, sent := attachedBridge(hitl)
	hitlservice.SetAskWatcher(hitl, bridge)

	resumed := make(chan string, 1)
	hitlservice.SetResumeHook(hitl, func(_ context.Context, approvalID string) error {
		resumed <- approvalID
		return nil
	})

	recorder := hitl.(hitlservice.ApprovalRecorder)
	if err := recorder.RecordPendingApproval(ctx, "ask-1", hitlservice.ApprovalRequest{
		ToolsName: "billing",
		ToolName:  "issue_refund",
		Args:      map[string]any{"path": "/tmp/refund.json"},
		SessionID: "cnx-sess-1",
		AgentName: "refund-desk",
		OnTimeout: hitlservice.ActionDeny,
	}); err != nil {
		t.Fatalf("RecordPendingApproval: %v", err)
	}

	f, err := librelay.Frame{Type: librelay.TypeAskVerdict, Instance: "inst-1"}.
		WithPayload(librelay.AskVerdict{AskID: "ask-1", Decision: librelay.AskDecisionAllow, DecidedBy: "u_9"})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	bridge.handleVerdict(ctx, f)

	select {
	case got := <-resumed:
		if got != "ask-1" {
			t.Fatalf("the resume hook fired for %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a relayed verdict never reached the resume hook a local answer runs")
	}
	bridge.detach()

	row, err := store.GetHITLApproval(ctx, "ask-1")
	if err != nil {
		t.Fatalf("GetHITLApproval: %v", err)
	}
	if row.State != runtimetypes.HITLApprovalApproved {
		t.Fatalf("state = %q, want approved", row.State)
	}

	frames := sent.all()
	if len(frames) != 2 {
		t.Fatalf("frames = %+v, want a publication and its retraction", frames)
	}
	if frames[0].Type != librelay.TypeAskPublished || frames[1].Type != librelay.TypeAskResolved {
		t.Fatalf("frames = %q then %q", frames[0].Type, frames[1].Type)
	}
	var resolved librelay.AskResolved
	if err := frames[1].DecodePayload(&resolved); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if resolved.AskID != "ask-1" || resolved.Reason != librelay.AskResolvedAnswered {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func waitFrames(t *testing.T, sent *capturedFrames, want int) []librelay.Frame {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		frames := sent.all()
		if len(frames) >= want {
			return frames
		}
		if time.Now().After(deadline) {
			t.Fatalf("frames = %d, want %d", len(frames), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func openAskRow(id string) *runtimetypes.HITLApproval {
	row := pendingAskRow()
	row.ID = id
	row.ExpiresAt = time.Now().UTC().Add(time.Hour)
	return row
}

func TestUnit_RelayAskBridge_RepublishesEveryStillOpenAskOnAConnection(t *testing.T) {
	t.Parallel()
	open, second := openAskRow("ask-open"), openAskRow("ask-open-2")
	answered := openAskRow("ask-answered")
	answered.State = runtimetypes.HITLApprovalApproved
	expired := openAskRow("ask-expired")
	expired.ExpiresAt = time.Now().UTC().Add(-time.Minute)

	inbox := &fakeAskInbox{pending: []*runtimetypes.HITLApproval{open, answered, expired, second}}
	b, sent := attachedBridge(inbox)
	b.republish(t.Context())
	frames := waitFrames(t, sent, 2)
	b.detach()

	frames = sent.all()
	var ids []string
	for _, f := range frames {
		if f.Type != librelay.TypeAskPublished {
			t.Fatalf("a re-publish sent %q", f.Type)
		}
		var published librelay.AskPublished
		if err := f.DecodePayload(&published); err != nil {
			t.Fatalf("DecodePayload: %v", err)
		}
		ids = append(ids, published.AskID)
	}
	if len(ids) != 2 || ids[0] != "ask-open" || ids[1] != "ask-open-2" {
		t.Fatalf("re-published %v, want only the two open asks", ids)
	}
}

func TestUnit_RelayAskBridge_ARepublishedAskIsIdenticalToItsFirstPublication(t *testing.T) {
	t.Parallel()
	row := openAskRow("ask-open")

	first, firstSent := attachedBridge(nil)
	first.AskRecorded(t.Context(), row)
	first.detach()

	again, againSent := attachedBridge(&fakeAskInbox{pending: []*runtimetypes.HITLApproval{row}})
	again.republish(t.Context())
	waitFrames(t, againSent, 1)
	again.detach()

	want, got := firstSent.all(), againSent.all()
	if len(want) != 1 || len(got) != 1 {
		t.Fatalf("frames = %d first, %d again, want one each", len(want), len(got))
	}
	if got[0].Type != want[0].Type || got[0].Instance != want[0].Instance ||
		got[0].Session != want[0].Session || got[0].ID != want[0].ID ||
		got[0].ReplyTo != want[0].ReplyTo || got[0].Seq != want[0].Seq ||
		got[0].Trace != want[0].Trace || string(got[0].Payload) != string(want[0].Payload) {
		t.Fatalf("re-publish = %+v, want the frame a first publication sends: %+v", got[0], want[0])
	}
}

func TestUnit_RelayAskBridge_RepublishNeedsALinkAndNeverFails(t *testing.T) {
	t.Parallel()
	inbox := &fakeAskInbox{pending: []*runtimetypes.HITLApproval{openAskRow("ask-open")}}

	detached := newRelayAskBridge(inbox, libtracker.NoopTracker{})
	detached.republish(t.Context())
	detached.detach()

	failing := &capturedFrames{err: relaylink.ErrNotConnected}
	b := newRelayAskBridge(inbox, libtracker.NoopTracker{})
	b.attach("inst-1", failing.send)
	b.republish(t.Context())
	b.detach()

	unreadable, _ := attachedBridge(&fakeAskInbox{err: errors.New("the database is gone")})
	unreadable.republish(t.Context())
	unreadable.detach()

	var nilBridge *relayAskBridge
	nilBridge.republish(t.Context())

	noInbox, noInboxSent := attachedBridge(nil)
	noInbox.republish(t.Context())
	noInbox.detach()
	if frames := noInboxSent.all(); len(frames) != 0 {
		t.Fatalf("a bridge with no inbox re-published %+v", frames)
	}
}

func TestUnit_RelayAskBridge_ARelayedDenialRecordsItsAuthorAndNote(t *testing.T) {
	ctx := t.Context()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "relay-verdict.db"), runtimetypes.SchemaSQLite)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{}, "")

	bridge, _ := attachedBridge(hitl)
	settled := make(chan string, 1)
	hitlservice.SetResumeHook(hitl, func(_ context.Context, approvalID string) error {
		settled <- approvalID
		return nil
	})

	recorder := hitl.(hitlservice.ApprovalRecorder)
	if err := recorder.RecordPendingApproval(ctx, "ask-1", hitlservice.ApprovalRequest{
		ToolsName: "billing",
		ToolName:  "issue_refund",
		OnTimeout: hitlservice.ActionDeny,
	}); err != nil {
		t.Fatalf("RecordPendingApproval: %v", err)
	}

	f, err := librelay.Frame{Type: librelay.TypeAskVerdict, Instance: "inst-1"}.
		WithPayload(librelay.AskVerdict{
			AskID:     "ask-1",
			Decision:  librelay.AskDecisionDeny,
			DecidedBy: "u_9",
			Guidance:  "Refund only up to 40 EUR.",
		})
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	bridge.handleVerdict(ctx, f)
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
		t.Fatal("the relayed verdict never reached the resume hook")
	}
	bridge.detach()

	row, err := store.GetHITLApproval(ctx, "ask-1")
	if err != nil {
		t.Fatalf("GetHITLApproval: %v", err)
	}
	if row.State != runtimetypes.HITLApprovalDenied {
		t.Fatalf("state = %q, want denied", row.State)
	}
	if by := hitlservice.DecidedByOf(row); by != "u_9" {
		t.Fatalf("decided by %q, want the supervisor the relay named", by)
	}
	if guidance := hitlservice.GuidanceOf(row); guidance != "Refund only up to 40 EUR." {
		t.Fatalf("guidance = %q, want the supervisor's note", guidance)
	}
}
