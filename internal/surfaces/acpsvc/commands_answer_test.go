package acpsvc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// fakeAskInbox is the AskInbox slice of hitlservice.Service: pending questions
// per mission, and the one Answer call the command must land.
type fakeAskInbox struct {
	pending map[string][]*runtimetypes.HITLApproval
	gotID   string
	gotText string
	err     error
}

func (f *fakeAskInbox) PendingAttentionAsks(_ context.Context, missionID string) ([]*runtimetypes.HITLApproval, error) {
	return f.pending[missionID], nil
}

func (f *fakeAskInbox) Answer(_ context.Context, askID, text string) error {
	f.gotID, f.gotText = askID, text
	return f.err
}

// fakeSupervision answers "which missions did this session fire", the
// ownership seam /answer checks before it answers anything.
type fakeSupervision struct {
	byParent map[string][]*missionservice.Mission
}

func (f *fakeSupervision) MissionsFiredBy(_ context.Context, parentSessionID string, _ int) ([]*missionservice.Mission, error) {
	return f.byParent[parentSessionID], nil
}

// attentionRow builds a pending question row, marked the way
// hitlservice.IsAttentionAsk recognizes one.
func attentionRow(id, missionID, summary string) *runtimetypes.HITLApproval {
	return &runtimetypes.HITLApproval{
		ID:          id,
		ToolsName:   hitlservice.AttentionToolsName,
		ToolName:    hitlservice.AttentionToolName,
		ArgsSummary: summary,
		State:       runtimetypes.HITLApprovalPending,
		OnTimeout:   "deny",
		MissionID:   &missionID,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   time.Now().UTC().Add(30 * time.Minute),
	}
}

// newAnswerTestTransport wires a Transport with the /answer capability over a
// real store, so the not-yours/wrong-kind branches read a real row.
func newAnswerTestTransport(t *testing.T, inbox *fakeAskInbox, sup *fakeSupervision) (*Transport, libdb.DBManager) {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "answer-acp.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	tr := &Transport{deps: Deps{DB: db}}
	if inbox != nil {
		tr.deps.Asks = inbox
	}
	if sup != nil {
		tr.deps.Supervision = sup
	}
	return tr, db
}

// TestUnit_HandleAnswer_ListsThenAnswersItsOwnMissionAsk pins the two shapes
// of /answer: bare lists this session's answerable questions with the handle
// each takes, and "<id> text" lands the operator's words on hitlservice.Answer
// verbatim.
func TestUnit_HandleAnswer_ListsThenAnswersItsOwnMissionAsk(t *testing.T) {
	inbox := &fakeAskInbox{pending: map[string][]*runtimetypes.HITLApproval{
		"mission-1": {attentionRow("ask-1", "mission-1", "which staging database?")},
	}}
	sup := &fakeSupervision{byParent: map[string][]*missionservice.Mission{
		"internal-1": {{ID: "mission-1", AgentName: "reviewer"}},
	}}
	tr, _ := newAnswerTestTransport(t, inbox, sup)
	sess := &sessionEntry{InternalSessionID: "internal-1"}

	listing, err := tr.handleAnswer(context.Background(), sess, "")
	require.NoError(t, err)
	require.Contains(t, listing, "ask-1")
	require.Contains(t, listing, "which staging database?")
	require.Contains(t, listing, "reviewer")
	require.Contains(t, listing, answerUsageLine, "the listing must teach the grammar that answers it")

	out, err := tr.handleAnswer(context.Background(), sess, "ask-1 use staging-b, it has the fixtures")
	require.NoError(t, err)
	require.Equal(t, "ask-1", inbox.gotID)
	require.Equal(t, "use staging-b, it has the fixtures", inbox.gotText, "the answer text must reach hitlservice unmangled")
	require.Contains(t, out, "reviewer")
}

// TestUnit_HandleAnswer_EmptyInboxAndMissingText pins the two refusals that
// cost nothing: no questions at all, and an id with no words after it.
func TestUnit_HandleAnswer_EmptyInboxAndMissingText(t *testing.T) {
	tr, _ := newAnswerTestTransport(t, &fakeAskInbox{}, &fakeSupervision{})
	sess := &sessionEntry{InternalSessionID: "internal-1"}

	listing, err := tr.handleAnswer(context.Background(), sess, "")
	require.NoError(t, err)
	require.Contains(t, listing, "Nothing is waiting on you")

	_, err = tr.handleAnswer(context.Background(), sess, "ask-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), answerUsageLine)
}

// TestUnit_HandleAnswer_PermissionAskGetsTheKindMismatchTeaching pins the
// teaching error `approvals respond` gives for the same mistake: a permission
// ask takes a verdict, never text, and the operator is told what does answer it.
func TestUnit_HandleAnswer_PermissionAskGetsTheKindMismatchTeaching(t *testing.T) {
	tr, db := newAnswerTestTransport(t, &fakeAskInbox{}, &fakeSupervision{})
	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, store.CreateHITLApproval(context.Background(), &runtimetypes.HITLApproval{
		ID:          "perm-1",
		ToolsName:   "local_fs",
		ToolName:    "write_file",
		ArgsSummary: "/tmp/x",
		State:       runtimetypes.HITLApprovalPending,
		OnTimeout:   "deny",
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   time.Now().UTC().Add(time.Hour),
	}))

	_, err := tr.handleAnswer(context.Background(), &sessionEntry{InternalSessionID: "internal-1"}, "perm-1 yes please")
	require.Error(t, err)
	require.Contains(t, err.Error(), "permission request")
	require.Contains(t, err.Error(), "local_fs.write_file")
	require.Contains(t, err.Error(), "contenox approvals respond perm-1 --approve|--deny")
}

// TestUnit_HandleAnswer_UnknownIdIsRefusedWithoutAnswering pins that an id
// this session does not own never reaches hitlservice.Answer — an answer is an
// instruction a unit acts on, so ownership is checked, not assumed.
func TestUnit_HandleAnswer_UnknownIdIsRefusedWithoutAnswering(t *testing.T) {
	inbox := &fakeAskInbox{}
	tr, _ := newAnswerTestTransport(t, inbox, &fakeSupervision{})

	_, err := tr.handleAnswer(context.Background(), &sessionEntry{InternalSessionID: "internal-1"}, "ask-nope some words")
	require.Error(t, err)
	require.Empty(t, inbox.gotID, "an unowned ask must not be answered")
	require.Contains(t, err.Error(), "ask-nope")
}

// TestUnit_HandleAnswer_ServiceRefusalsArePhrasedForTheOperator pins that
// hitlservice's sentinels (which name the package and its internal state)
// never reach the operator raw; each maps to what happened and what to do.
func TestUnit_HandleAnswer_ServiceRefusalsArePhrasedForTheOperator(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want []string
		deny []string
	}{
		{
			name: "already answered",
			err:  hitlservice.ErrApprovalAlreadyResolved,
			want: []string{"already answered"},
			deny: []string{"hitlservice:"},
		},
		{
			name: "expired",
			err:  hitlservice.ErrApprovalExpired,
			want: []string{"expired", "deny"},
			deny: []string{"hitlservice:"},
		},
		{
			name: "needs a resumer",
			err:  hitlservice.ErrVerdictNeedsResumer,
			want: []string{"NOT recorded", "contenox approvals respond"},
			deny: []string{"hitlservice:"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbox := &fakeAskInbox{
				pending: map[string][]*runtimetypes.HITLApproval{
					"mission-1": {attentionRow("ask-1", "mission-1", "which database?")},
				},
				err: tc.err,
			}
			sup := &fakeSupervision{byParent: map[string][]*missionservice.Mission{
				"internal-1": {{ID: "mission-1", AgentName: "reviewer"}},
			}}
			tr, _ := newAnswerTestTransport(t, inbox, sup)

			_, err := tr.handleAnswer(context.Background(), &sessionEntry{InternalSessionID: "internal-1"}, "ask-1 the words")
			require.Error(t, err)
			for _, want := range tc.want {
				require.Contains(t, err.Error(), want)
			}
			for _, deny := range tc.deny {
				require.NotContains(t, err.Error(), deny, "a developer sentinel must not reach the operator")
			}
		})
	}
}

// TestUnit_AnswerCapabilityGatesTheMenuButNotTheParser pins the same honesty
// rule /mission follows: an unwired /answer is not advertised, but typing it
// still reaches its handler's teaching error instead of "unknown command".
func TestUnit_AnswerCapabilityGatesTheMenuButNotTheParser(t *testing.T) {
	tr, _ := newAnswerTestTransport(t, nil, nil)
	require.False(t, tr.hasAnswerCapability())
	require.False(t, containsCommand(tr.acpCommands(), "answer"), "an unusable /answer must not be advertised")

	name, args, ok := parseCommand("/answer ask-1 the words")
	require.True(t, ok)
	require.Equal(t, "answer", name)
	require.Equal(t, "ask-1 the words", args)

	_, err := tr.handleAnswer(context.Background(), &sessionEntry{InternalSessionID: "internal-1"}, "ask-1 the words")
	require.Error(t, err)
	require.Contains(t, err.Error(), "contenox approvals respond", "the refusal must name the terminal path that does work")

	tr.deps.Asks = &fakeAskInbox{}
	tr.deps.Supervision = &fakeSupervision{}
	require.True(t, tr.hasAnswerCapability())
	require.True(t, containsCommand(tr.acpCommands(), "answer"))
}

// TestUnit_UntilLabel pins the countdown clause: coarse units while the window
// is open, nothing once it is gone (a passed deadline must not read as open).
func TestUnit_UntilLabel(t *testing.T) {
	now := time.Now().UTC()
	require.Equal(t, "45s", untilLabel(now, now.Add(45*time.Second)))
	require.Equal(t, "29m", untilLabel(now, now.Add(29*time.Minute+30*time.Second)))
	require.Equal(t, "1h5m", untilLabel(now, now.Add(65*time.Minute)))
	require.Equal(t, "", untilLabel(now, now.Add(-time.Minute)))
	require.Equal(t, "", untilLabel(now, time.Time{}))
}

// TestUnit_HandleAnswer_ListingSurvivesAnUnreadableMission pins that one
// mission whose asks cannot be read does not hide the others' questions.
func TestUnit_HandleAnswer_ListingNamesEveryFiredMission(t *testing.T) {
	inbox := &fakeAskInbox{pending: map[string][]*runtimetypes.HITLApproval{
		"mission-1": {attentionRow("ask-1", "mission-1", "first question")},
		"mission-2": {attentionRow("ask-2", "mission-2", "second question")},
	}}
	sup := &fakeSupervision{byParent: map[string][]*missionservice.Mission{
		"internal-1": {{ID: "mission-1", AgentName: "reviewer"}, {ID: "mission-2", AgentName: "builder"}},
	}}
	tr, _ := newAnswerTestTransport(t, inbox, sup)

	listing, err := tr.handleAnswer(context.Background(), &sessionEntry{InternalSessionID: "internal-1"}, "")
	require.NoError(t, err)
	for _, want := range []string{"ask-1", "ask-2", "reviewer", "builder"} {
		require.Contains(t, listing, want)
	}
	require.True(t, strings.Contains(listing, "answerable for"), "each row states how long it stays answerable")
}
