package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/modelrepo/scriptedtest"
	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/internal/surfaces/beam/app"
	"github.com/contenox/contenox/internal/surfaces/beam/enginebridge"
	"github.com/contenox/contenox/internal/surfaces/beam/frame"
	"github.com/contenox/contenox/internal/surfaces/beam/input"
	"github.com/contenox/contenox/internal/surfaces/beam/style"
	"github.com/contenox/contenox/internal/surfaces/beam/term"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// beamHost is one `contenox beam` process's worth of runtime: the real acpsvc
// transport on one end of a pipe, the real enginebridge on the other, and the
// one workspace root that host was launched in.
type beamHost struct {
	t         *testing.T
	root      string
	transport *acpsvc.Transport
	bridge    *enginebridge.Bridge
	stop      func()
}

// startBeamHost composes what runBeamSurface composes: a workspace-scoped
// transport (the beam profile's shape, WorkspaceRoots fixed at launch) speaking
// ACP over an in-memory pipe to the real bridge beam's app-shell drives.
func startBeamHost(t *testing.T, db libdb.DBManager, engine *enginesvc.Engine, deps acpsvc.Deps, root string) *beamHost {
	t.Helper()

	factory, err := buildWorkspaceFactory(root)
	require.NoError(t, err)
	deps.Engine = engine
	deps.DB = db
	deps.WorkspaceRoots = factory

	runCtx, cancel := context.WithCancel(context.Background())

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &duplexPipe{r: agentR, w: agentW}
	clientSide := &duplexPipe{r: clientR, w: clientW}

	var transport *acpsvc.Transport
	build := acpsvc.New(deps)
	agentConn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		agent := build(c)
		transport, _ = agent.(*acpsvc.Transport)
		return agent
	})
	require.NotNil(t, transport, "the ACP factory must produce a transport")

	agentDone := make(chan error, 1)
	go func() { agentDone <- agentConn.Run(runCtx) }()

	bridge, err := enginebridge.New(runCtx, enginebridge.Deps{
		Conn:          clientSide,
		ClientInfo:    &libacp.Implementation{Name: "beam", Version: "system-test"},
		WorkspaceRoot: root,
	})
	require.NoError(t, err)

	initCtx, initCancel := context.WithTimeout(runCtx, systemTestBudget)
	defer initCancel()
	_, err = bridge.Initialize(initCtx)
	require.NoError(t, err, "the ACP handshake beam performs before it resolves a session")

	h := &beamHost{t: t, root: root, transport: transport, bridge: bridge}
	var once sync.Once
	h.stop = func() {
		once.Do(func() {
			_ = bridge.Close()
			closeCtx, closeCancel := context.WithTimeout(context.Background(), systemTestBudget)
			defer closeCancel()
			_ = transport.Close(closeCtx)
			cancel()
			select {
			case <-agentDone:
			case <-time.After(systemTestBudget):
			}
		})
	}
	t.Cleanup(h.stop)
	return h
}

// systemTestBudget bounds every wait in these tests. It is generous against
// loaded CI and still short enough that a stall fails instead of hanging.
const systemTestBudget = 30 * time.Second

// resolveSession runs exactly what `contenox beam [path]` runs to decide which
// session the operator lands in.
func (h *beamHost) resolveSession(args ...string) (libacp.SessionID, bool) {
	h.t.Helper()
	cmd := &cobra.Command{Use: "beam"}
	cmd.Flags().String("session", "", "")
	cmd.Flags().Bool("new", false, "")
	require.NoError(h.t, cmd.ParseFlags(args))

	ctx, cancel := context.WithTimeout(context.Background(), systemTestBudget)
	defer cancel()
	id, fresh, err := resolveBeamSession(ctx, cmd, h.bridge, h.root)
	require.NoError(h.t, err)
	return id, fresh
}

func systemTestDB(t *testing.T) libdb.DBManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(),
		filepath.Join(t.TempDir(), "beam-system.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func workspaceDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)
	return abs
}

// seedForeignSession writes a named, freshly-messaged ACP session straight into
// the shared store, with no workspace root recorded for it — what a session
// another surface (or an older contenox) left on this machine. It is newer than
// anything beam creates during the test, so a roster that is not workspace-scoped
// hands it back first.
func seedForeignSession(t *testing.T, db libdb.DBManager, workspaceID, name string) {
	t.Helper()
	ctx := context.Background()
	exec := db.WithoutTransaction()
	internalID := "idx-" + uuid.NewString()
	require.NoError(t, runtimetypes.NewMessageStore(exec, workspaceID).
		CreateNamedMessageIndex(ctx, internalID, acpsvc.ClientIdentity, name))
	require.NoError(t, chatservice.NewManager(workspaceID).PersistDiff(ctx, exec, internalID,
		[]taskengine.Message{{Role: "user", Content: "left behind by another surface", Timestamp: time.Now()}}))
}

// TestSystem_Beam_ResumesTheSessionForThisWorkspace pins beam's front door:
// which session `contenox beam` lands you in is a property of the directory you
// are standing in, never of the machine. One store holds a crowd of newer
// sessions belonging to nobody's workspace, plus a second workspace's own; beam
// in A must ignore all of them, and beam in B must not inherit A's.
func TestSystem_Beam_ResumesTheSessionForThisWorkspace(t *testing.T) {
	db := systemTestDB(t)
	engine := &enginesvc.Engine{}
	const workspaceID = "beam-scope-ws"
	deps := acpsvc.Deps{WorkspaceID: workspaceID}

	// More than one roster page, so a scope applied after pagination cannot see
	// this workspace's session at all.
	for i := 0; i < 105; i++ {
		seedForeignSession(t, db, workspaceID, fmt.Sprintf("stray-%03d", i))
	}

	workA := workspaceDir(t, "workspace-a")
	workB := workspaceDir(t, "workspace-b")

	hostA := startBeamHost(t, db, engine, deps, workA)
	sessionA, freshA := hostA.resolveSession()
	require.True(t, freshA,
		"with no session of its own, beam in workspace A starts one instead of adopting a stranger's")
	require.NotEmpty(t, sessionA)
	hostA.stop()

	hostB := startBeamHost(t, db, engine, deps, workB)
	sessionB, freshB := hostB.resolveSession()
	require.True(t, freshB,
		"beam launched in workspace B must start its own session, not replay one rooted in A")
	require.NotEqual(t, sessionA, sessionB,
		"beam in workspace B resumed workspace A's session: the newest session on the machine is not this workspace's session")
	hostB.stop()

	againA := startBeamHost(t, db, engine, deps, workA)
	resumedA, freshAgainA := againA.resolveSession()
	require.False(t, freshAgainA,
		"returning to workspace A reopens its session; a store full of newer foreign sessions must not hide it")
	require.Equal(t, sessionA, resumedA,
		"beam must land back in the session belonging to the directory it was started in")
	againA.stop()

	againB := startBeamHost(t, db, engine, deps, workB)
	resumedB, freshAgainB := againB.resolveSession()
	require.False(t, freshAgainB)
	require.Equal(t, sessionB, resumedB,
		"workspace B reopens B's session even though A's is newer in the shared store")
}

// askEverything is the shape both approval tests run under: no rule matches, so
// every gated call falls to default_action, and default_action is "approve" —
// the ask-everything envelope a fresh install ships.
const askEverything = `{"default_action":"approve","rules":[]}`

// commitMessageDialog is the maintainer's ten-second path, scripted: route the
// turn, call native-git's git_diff, then answer from its result.
const commitMessageDialog = `{
  "model": "scripted-test",
  "turns": [
    {"text": "general"},
    {
      "text": "Let me look at what changed.",
      "tool_calls": [{"name": "git_diff", "arguments": {}}]
    },
    {"text": "Suggested commit message: tighten the greeting copy."}
  ]
}`

const scriptedFinalAnswer = "Suggested commit message: tighten the greeting copy."

// beamRuntime is a whole `contenox beam` in one process: a real workspace with
// a real git repo, the real engine on the scripted-test backend, the real HITL
// service over the real store, and the real ACP transport the beam bridge talks
// to. Only the model is replaced.
type beamRuntime struct {
	t           *testing.T
	db          libdb.DBManager
	hitl        hitlservice.Service
	workspace   string
	contenoxDir string
	host        *beamHost
	sessionID   libacp.SessionID
}

// newBeamRuntime builds that runtime. dialog is the scripted-test script the
// model turns are replayed from; policy is the HITL envelope every gated call is
// evaluated against.
func newBeamRuntime(t *testing.T, dialog, policy string) *beamRuntime {
	t.Helper()
	ctx := context.Background()

	// A private HOME keeps the operator's own ~/.contenox out of the policy
	// search path and the chain search path.
	t.Setenv("HOME", t.TempDir())

	workspace := workspaceDir(t, "workspace")
	newTestGitRepo(t, workspace)

	contenoxDir := filepath.Join(workspace, ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "hitl-policy-default.json"), []byte(policy), 0o644))

	scriptPath := filepath.Join(contenoxDir, "dialog.json")
	require.NoError(t, os.WriteFile(scriptPath, []byte(dialog), 0o644))

	db := systemTestDB(t)
	require.NoError(t, runtimetypes.New(db.WithoutTransaction()).CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "scripted-backend",
		Name:    "scripted",
		Type:    modelrepo.ScriptedTestBackendType,
		BaseURL: scriptPath,
	}))

	// Late-bound exactly as acp_cmd.go binds it: the toolset's cwd resolver and
	// the approval router both read the transport the connection produces.
	var transport *acpsvc.Transport
	transportFn := func() *acpsvc.Transport { return transport }

	hitl := newHITLService(ctx, contenoxDir, runtimetypes.New(db.WithoutTransaction()), libtracker.NoopTracker{}, "")
	router := acpsvc.NewSessionRouter()

	engine, err := BuildEngine(ctx, db, chatOpts{
		EffectiveDefaultModel:    scriptedtest.DefaultModelName,
		EffectiveDefaultProvider: modelrepo.ScriptedTestBackendType,
		ContenoxDir:              contenoxDir,
		EffectiveHITL:            true,
		EffectiveHITLService:     hitl,
		EffectiveAskApproval:     routedAskApproval(router, transportFn),
		EffectiveExtraTools: map[string]taskengine.ToolsRepo{
			localtools.GitToolsName: localtools.NewGitToolsWith("", localtools.GitToolsName,
				acpsvc.NewACPCwdResolver(func(context.Context) *acpsvc.Transport { return transport })),
		},
	})
	require.NoError(t, err)
	t.Cleanup(engine.Stop)

	t.Setenv("CONTENOX_ACP_CHAIN_PATH", generatedACPChain(t))
	chains, err := acpsvc.LoadChainRegistryFrom("chain-agent-acp.json", "CONTENOX_ACP_CHAIN_PATH")
	require.NoError(t, err)

	host := startBeamHost(t, db, engine, acpsvc.Deps{
		ChainRegistry:   chains,
		DefaultModel:    scriptedtest.DefaultModelName,
		DefaultProvider: modelrepo.ScriptedTestBackendType,
		WorkspaceID:     ResolveWorkspaceID(contenoxDir),
		ContenoxDir:     contenoxDir,
		SessionRouter:   router,
		Asks:            hitl,
	}, workspace)
	transport = host.transport

	sessionID, fresh := host.resolveSession()
	require.True(t, fresh)
	host.bridge.SetActiveSession(sessionID)

	return &beamRuntime{
		t: t, db: db, hitl: hitl,
		workspace: workspace, contenoxDir: contenoxDir, host: host, sessionID: sessionID,
	}
}

// newTestGitRepo makes dir a git repository with one committed file and an
// uncommitted edit to it, so git_diff has something real to report.
func newTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	_, err = wt.Add("README.md")
	require.NoError(t, err)
	_, err = wt.Commit("initial commit", &git.CommitOptions{Author: &object.Signature{
		Name: "Beam System Test", Email: "beam@example.com", When: time.Now(),
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello there\n"), 0o644))
}

// generatedACPChain renders the shipped ACP agent declaration into the chain
// beam actually runs, so these tests traverse the real routing and tool loop.
func generatedACPChain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_, err := agentdecl.Preseed(dir)
	require.NoError(t, err)
	cfg, err := agentdecl.Shipped()
	require.NoError(t, err)
	generated := filepath.Join(dir, agentdecl.GeneratedDirName)
	_, err = agentdecl.Sync([]agentdecl.SourceDir{{
		Path:   filepath.Join(dir, agentdecl.NativeSourceDir),
		Native: true,
	}}, generated, cfg)
	require.NoError(t, err)
	return filepath.Join(generated, "chain-agent-acp.json")
}

// beamScreen is beam's terminal, headless: it hands the app-shell the keystrokes
// a test types and keeps every line the app ever committed.
type beamScreen struct {
	events chan input.Event

	mu   sync.Mutex
	back strings.Builder
	live string
}

var _ term.Engine = (*beamScreen)(nil)

func newBeamScreen() *beamScreen {
	return &beamScreen{events: make(chan input.Event, 256)}
}

func (s *beamScreen) Events() <-chan input.Event { return s.events }

func (s *beamScreen) Commit(f frame.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range f.Scrollback {
		s.back.WriteString(l.Text())
		s.back.WriteByte('\n')
	}
	var live strings.Builder
	for _, l := range f.Live {
		live.WriteString(l.Text())
		live.WriteByte('\n')
	}
	s.live = live.String()
	return nil
}

func (s *beamScreen) Size() (int, int)                     { return 120, 40 }
func (s *beamScreen) Suspend(fn func() error) error        { return fn() }
func (s *beamScreen) Bell()                                {}
func (s *beamScreen) CopyToClipboard(string) (bool, error) { return false, nil }
func (s *beamScreen) Close() error                         { return nil }

// text is everything the operator can see right now: the transcript that
// scrolled by, plus the live region still on screen.
func (s *beamScreen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.back.String() + s.live
}

// liveText is only the region beam repaints in place — where a modal card sits
// while it owns the keyboard.
func (s *beamScreen) liveText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// beamSession is a running beam surface: the real app-shell over the real
// bridge, driven by keystrokes.
type beamSession struct {
	t      *testing.T
	screen *beamScreen
	done   chan error
	cancel context.CancelFunc
}

// openBeam starts the beam app-shell on this runtime's session, exactly as
// driveBeam starts it, with a headless terminal in place of the tty.
func (r *beamRuntime) openBeam() *beamSession {
	r.t.Helper()
	screen := newBeamScreen()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, app.Deps{
			Term:         screen,
			Bridge:       r.host.bridge,
			Caps:         style.Caps{Profile: style.ProfileMono, Dark: true},
			SessionID:    r.sessionID,
			Cwd:          r.workspace,
			FreshSession: true,
			Model:        scriptedtest.DefaultModelName,
			Provider:     modelrepo.ScriptedTestBackendType,
			SessionName:  string(r.sessionID),
		})
	}()
	b := &beamSession{t: r.t, screen: screen, done: done, cancel: cancel}
	r.t.Cleanup(b.close)
	return b
}

func (b *beamSession) close() {
	b.cancel()
	select {
	case <-b.done:
	case <-time.After(systemTestBudget):
		b.t.Error("beam did not shut down")
	}
}

func (b *beamSession) key(ev input.Event) {
	b.t.Helper()
	select {
	case b.screen.events <- ev:
	case <-time.After(systemTestBudget):
		b.t.Fatal("beam stopped reading the keyboard")
	}
}

// submit types a prompt and presses Enter, the way an operator sends a turn.
func (b *beamSession) submit(text string) {
	b.t.Helper()
	for _, r := range text {
		b.key(input.KeyEvent{Key: input.KeyRune, Rune: r})
	}
	b.key(input.KeyEvent{Key: input.KeyEnter})
}

// allow presses the one key that answers an approval card with "yes".
func (b *beamSession) allow() {
	b.t.Helper()
	b.key(input.KeyEvent{Key: input.KeyRune, Rune: 'y'})
}

// waitFor blocks until want shows up on screen, failing with the whole
// transcript when it never does — a stall reads as a failure, not a hang.
func (b *beamSession) waitFor(want, why string) {
	b.t.Helper()
	deadline := time.Now().Add(systemTestBudget)
	for time.Now().Before(deadline) {
		if strings.Contains(b.screen.text(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
		select {
		case err := <-b.done:
			b.done <- err
			b.t.Fatalf("beam exited before %q appeared (%s): %v\n--- screen ---\n%s", want, why, err, b.screen.text())
		default:
		}
	}
	b.t.Fatalf("timed out waiting for %q: %s\n--- screen ---\n%s", want, why, b.screen.text())
}

// requireCardRetired pins that the modal is gone from the live region: a card
// that is answered but still on screen still owns the keyboard, which is a stall
// the operator cannot type their way out of.
func (b *beamSession) requireCardRetired() {
	b.t.Helper()
	deadline := time.Now().Add(systemTestBudget)
	for time.Now().Before(deadline) {
		if !strings.Contains(b.screen.liveText(), "approval required") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b.t.Fatalf("the approval card is still holding the live region after the turn finished\n--- screen ---\n%s", b.screen.text())
}

func (b *beamSession) refute(unwanted, why string) {
	b.t.Helper()
	require.NotContains(b.t, b.screen.text(), unwanted, why)
}

// waitForPendingAsk returns the durable ask the gated call raised. The row is
// what an answer from anywhere lands on, so it exists before any card does.
func (r *beamRuntime) waitForPendingAsk() *runtimetypes.HITLApproval {
	r.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(systemTestBudget)
	for time.Now().Before(deadline) {
		pending, err := r.hitl.ListPending(ctx, 10)
		require.NoError(r.t, err)
		if len(pending) == 1 {
			return pending[0]
		}
		require.LessOrEqual(r.t, len(pending), 1, "one gated call raises exactly one ask")
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatal("the gated tool call never wrote a durable approval row")
	return nil
}

// requireApprovedAndClosed pins that the ask ended as an answered row, not as
// one still sitting in the inbox. The row is closed by whichever goroutine held
// the gate, so this waits for it rather than reading once.
func (r *beamRuntime) requireApprovedAndClosed(askID string) {
	r.t.Helper()
	ctx := context.Background()
	store := runtimetypes.New(r.db.WithoutTransaction())

	var row *runtimetypes.HITLApproval
	require.Eventually(r.t, func() bool {
		got, err := store.GetHITLApproval(ctx, askID)
		if err != nil {
			return false
		}
		row = got
		return row.State != runtimetypes.HITLApprovalPending
	}, systemTestBudget, 20*time.Millisecond, "the durable row never left the pending state")

	require.Equal(r.t, runtimetypes.HITLApprovalApproved, row.State,
		"the durable row must carry the verdict, whoever entered it")
	pending, err := r.hitl.ListPending(ctx, 10)
	require.NoError(r.t, err)
	require.Empty(r.t, pending, "an answered ask must leave the pending inbox")
}

// requireGatedToolRan proves the gate opened rather than merely closing: the
// session's persisted transcript must carry the real git tool's output, read off
// the real working tree, as the tool result the model then answered from. A
// denied or never-run call leaves a refusal there instead. The transcript is
// written as the turn settles, after its text is already on screen, so this
// waits for the row rather than racing it.
func (r *beamRuntime) requireGatedToolRan() {
	r.t.Helper()
	ctx := context.Background()
	exec := r.db.WithoutTransaction()
	workspaceID := ResolveWorkspaceID(r.contenoxDir)

	var internalID string
	require.NoError(r.t, exec.QueryRowContext(ctx,
		`SELECT id FROM message_indices WHERE name = $1 AND workspace_id = $2 AND identity = 'acp-client'`,
		string(r.sessionID), workspaceID).Scan(&internalID))

	var toolResults []string
	require.Eventually(r.t, func() bool {
		msgs, err := chatservice.NewManager(workspaceID).ListMessages(ctx, exec, internalID)
		if err != nil {
			return false
		}
		toolResults = nil
		for _, m := range msgs {
			if m.Role == "tool" {
				toolResults = append(toolResults, m.Content)
			}
		}
		return len(toolResults) > 0
	}, systemTestBudget, 20*time.Millisecond, "the turn recorded no tool result at all")

	require.Len(r.t, toolResults, 1, "the turn ran exactly one gated tool call")
	require.Contains(r.t, toolResults[0], "+hello there",
		"the approved call must have executed and fed the working tree's real diff back into the turn")
}

// TestSystem_Beam_AnsweredApprovalContinuesTheTurn is the ten-second path that
// broke: ask beam for a commit message, it reaches for native-git's git_diff,
// the policy's default_action gates the call, the operator presses y — and the
// turn CONTINUES in place. The tool runs, the scripted answer arrives, and the
// turn never announces itself suspended.
func TestSystem_Beam_AnsweredApprovalContinuesTheTurn(t *testing.T) {
	rt := newBeamRuntime(t, commitMessageDialog, askEverything)
	beam := rt.openBeam()

	beam.submit("can you suggest a commit message?")

	ask := rt.waitForPendingAsk()
	require.Equal(t, "git_diff", ask.ToolName,
		"the ask must name the gated call, since no rule matched and default_action asks")
	beam.waitFor("approval required", "a gated tool call must reach the operator as a card")

	beam.allow()

	beam.waitFor(scriptedFinalAnswer,
		"answering the card must carry the turn on: the gated tool runs and the model answers from its result")
	beam.refute("The turn is suspended, not finished",
		"an ask answered by the operator who is sitting right there must never park the turn")
	require.Contains(t, beam.screen.text(), "allowed",
		"the verdict belongs in the transcript, so a gate that opened leaves a recorded reason")
	beam.requireCardRetired()

	rt.requireGatedToolRan()
	rt.requireApprovedAndClosed(ask.ID)
}

// TestSystem_Approval_AnsweredFromAnotherProcessReleasesTheBlockedTurn pins the
// other half of the same contract: the ask is a durable row, so the verdict may
// arrive from anywhere — a phone over the relay, `contenox approvals respond` in
// a second terminal, an adjudicating agent. Nothing here touches beam's card;
// the verdict is written by a second hitlservice over the same store, with no
// waiter parked and no resume hook, and the blocked turn must still continue.
func TestSystem_Approval_AnsweredFromAnotherProcessReleasesTheBlockedTurn(t *testing.T) {
	rt := newBeamRuntime(t, commitMessageDialog, askEverything)
	beam := rt.openBeam()

	beam.submit("can you suggest a commit message?")

	ask := rt.waitForPendingAsk()
	beam.waitFor("approval required", "the card is raised beside the row, and stays unanswered here")

	elsewhere := hitlservice.NewWithDefaultPolicy(
		hitlPolicySource(rt.contenoxDir),
		runtimetypes.LocalTenantID,
		runtimetypes.New(rt.db.WithoutTransaction()),
		libtracker.NoopTracker{},
		"",
	)
	require.NoError(t, elsewhere.Respond(context.Background(), ask.ID, true),
		"a process that did not raise the ask must still be able to answer its row")

	beam.waitFor(scriptedFinalAnswer,
		"a verdict written to the row from another process must release the turn blocked on it")
	beam.refute("The turn is suspended, not finished",
		"the turn was waiting on the row, so an answer on the row continues it rather than parking it")
	beam.requireCardRetired()

	rt.requireGatedToolRan()
	rt.requireApprovedAndClosed(ask.ID)
}
