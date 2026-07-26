package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/operatorinbox"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// TestSystem_ACPMissionInProcess is the house-idiom ACCEPTANCE for the ontology-
// correcting refit: a
// standalone `contenox acp` editor — what a Zed/ACP-registry user launches, with
// NO serve anywhere — embeds the fleet IN-PROCESS and fires `/mission` as a
// SUBAGENT of ITSELF. It runs END TO END against the real binary, driven over
// stdio by a real libacp client, and proves the whole journey the serve-forwarded
// topology could not:
//
//	no serve       → /mission IS advertised on a fresh session;
//	invoke it      → the editor's embedded kernel dispatches a REAL child
//	                 subprocess (a mission unit), which files a report through its
//	                 mission_report tool;
//	the report     → is delivered LIVE into the FIRING session's own stream —
//	                 the `contenox.missionReport` _meta update arrives at the
//	                 client, NOT the operator inbox (the fix: the firing session
//	                 lives in THIS process, so its report router reaches it);
//	the mission    → is a durable record in the shared db;
//	kill the editor→ reaps the child (no orphan) — mission lifetime ≤ process
//	                 lifetime.
//
// Hermetic like the forwarding e2e: an isolated HOME, `contenox init`, a
// deterministic no-model chain-agent fixture, a fake default model so any
// accidental resolution fails loudly. No LLM, GPU, or network.

// inprocReporterChain is the deterministic, model-free chain the dispatched CHILD
// runs as its first and only turn: it files a RESULT report through its granted
// mission tool, then a noop terminator — no model touched, and no mission_finish,
// so the unit stays a live idle ACP server (proving reaping kills it, not a
// self-exit).
const inprocReporterChain = `{
  "id": "inproc-reporter-chain",
  "description": "In-process e2e: file a result report, then a noop terminator.",
  "tasks": [
    {
      "id": "report",
      "handler": "tools",
      "tools": {"name": "mission", "tool_name": "mission_report", "args": {"kind": "result", "summary": "in-process unit reporting home"}},
      "transition": {"branches": [{"operator": "default", "goto": "done"}]}
    },
    {"id": "done", "handler": "noop", "transition": {"branches": [{"operator": "default", "goto": "end"}]}}
  ]
}`

const (
	inprocAgentName  = "inproc-reporter"
	inprocIntent     = "run the in-process mission and report home"
	inprocReportText = "in-process unit reporting home"
)

func TestSystem_ACPMissionInProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping acp in-process mission system e2e: builds contenox and spawns a real acp + child subprocess")
	}

	bin := fwdBuildBin(t)

	root := t.TempDir()
	homeDir := filepath.Join(root, "home")
	workspaceDir := filepath.Join(root, "workspace")
	dataDir := filepath.Join(workspaceDir, ".contenox")
	dbPath := filepath.Join(homeDir, ".contenox", "local.db")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))

	// NOTE: CONTENOX_SERVER_URL is deliberately EMPTY — no forwarding, no serve.
	// The editor must embed the fleet in-process. CONTENOX_ACP_CHAIN_PATH is empty
	// too, so THIS process is a top-level editor (not a dispatched unit) and hosts
	// the fleet; the child unit gets the chain path from its declared agent env.
	baseEnv := append(os.Environ(),
		"HOME="+homeDir,
		"CONTENOX_DEFAULT_MODEL=inproc-e2e-fake-model",
		"CONTENOX_DEFAULT_PROVIDER=ollama",
		"CONTENOX_SERVER_URL=",
		"CONTENOX_SERVER_TOKEN=",
		"CONTENOX_ACP_CHAIN_PATH=",
	)

	fwdRunCLI(t, bin, baseEnv, "--data-dir", dataDir, "--db", dbPath, "init", "--force")

	// The child's chain goes in a PLAIN directory — NOT under .contenox, which
	// control-plane isolation refuses to discover from — read via the agent's env.
	chainsDir := filepath.Join(root, "chains")
	require.NoError(t, os.MkdirAll(chainsDir, 0o755))
	chainPath := filepath.Join(chainsDir, "inproc-reporter.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(inprocReporterChain), 0o644))

	inprocSeed(t, dbPath, bin, chainPath)

	// A handle to the ONE shared store for assertions (the mission record lands
	// here; the report is delivered live rather than inboxed, which we also assert).
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	missions := missionservice.New(db)
	inbox := operatorinbox.New(db)

	// ── Spawn `contenox acp` as Zed would — NO serve, no CONTENOX_SERVER_URL ─────
	h, cmd, shutdown := inprocSpawnACP(t, bin, baseEnv)
	editorPID := cmd.Process.Pid

	_, err = h.client.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "zed", Version: "e2e"},
	})
	require.NoErrorf(t, err, "acp initialize failed\nacp stderr:\n%s", h.stderr())

	projectDir := filepath.Join(workspaceDir, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	// ── /mission IS advertised with the in-process fleet, no serve anywhere ──────
	sid, cmds := h.newSessionCommands(t, ctx, projectDir)
	require.Containsf(t, cmds, "mission",
		"/mission must be advertised by an editor that embeds the fleet in-process\nacp stderr:\n%s", h.stderr())

	// ── Invoking /mission dispatches a REAL child IN THIS process ────────────────
	confirmation := h.promptFor(t, ctx, sid, "/mission "+inprocAgentName+" "+inprocIntent)
	require.Contains(t, confirmation, "Mission fired", "the in-process fire is confirmed")
	require.Contains(t, confirmation, inprocAgentName, "the confirmation names the fired agent")
	require.Contains(t, confirmation, "live in this session",
		"the IN-PROCESS confirmation must promise live delivery into this session, not the inbox as the primary home")

	// ── The child's report is delivered LIVE into the firing session's stream ────
	// This is the whole point of the refit: the report router reaches THIS session
	// (the firing editor session lives in this process), so the update — carrying
	// the contenox.missionReport _meta — arrives at the client, not the inbox.
	reportUpdate := waitForMissionReport(t, h, inprocReportText)
	require.Equal(t, sid, reportUpdate.SessionID,
		"the live report is delivered on the FIRING session the client knows")

	// ── The mission is a durable record in the shared db ─────────────────────────
	var mission *missionservice.Mission
	require.Eventuallyf(t, func() bool {
		ms, lerr := missions.List(ctx, nil, 100)
		if lerr != nil {
			return false
		}
		for _, m := range ms {
			if m.AgentName == inprocAgentName && m.Intent == inprocIntent {
				mission = m
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond,
		"the fired mission must be a durable record in the shared db\nacp stderr:\n%s", h.stderr())
	require.NotEmpty(t, mission.ParentSessionID,
		"the firing session id rode the dispatch as the supervision edge")

	// ── The live-delivered report did NOT also fall into the operator inbox ──────
	// A supervised report has a home; the inbox is only the no-live-parent fallback.
	inboxItems, err := inbox.List(ctx, 100)
	require.NoError(t, err)
	for _, it := range inboxItems {
		require.NotEqualf(t, mission.ID, it.MissionID,
			"a report delivered live to its firing session must NOT also land in the operator inbox (mission %s)", mission.ID)
	}

	// ── Killing the editor reaps the child (no orphan) ───────────────────────────
	// The dispatched unit is a live child subprocess of the editor. Capture it,
	// then shut the editor down (closing its stdin — the Zed-disconnect path, which
	// drives the graceful teardown: conn.Run returns, the deferred kernel.Close
	// stops every instance) and prove the child does not outlive its parent — the
	// ontology's "mission lifetime ≤ acp process lifetime".
	var kids []int
	require.Eventuallyf(t, func() bool {
		kids = childPIDs(editorPID)
		return len(kids) > 0
	}, 30*time.Second, 200*time.Millisecond,
		"the dispatched mission unit must be a live child of the editor process\nacp stderr:\n%s", h.stderr())

	shutdown()
	waitProcess(t, cmd, 20*time.Second, h)

	require.Eventuallyf(t, func() bool {
		for _, pid := range kids {
			if pidAlive(pid) {
				return false
			}
		}
		return true
	}, 15*time.Second, 200*time.Millisecond,
		"the editor's death must reap its dispatched unit(s) — an orphan child remains\nacp stderr:\n%s", h.stderr())
}

// ── helpers ─────────────────────────────────────────────────────────────────

// ── ACP client harness over the acp subprocess's stdio ──────────────────────

// fwdACPClient captures every session/update notification in wire order, so the
// test can assert the advertised command menu and the /mission confirmation the
// way a real editor renders them.
type fwdACPClient struct {
	libacp.UnimplementedClient
	updates chan libacp.SessionNotification
}

func (c *fwdACPClient) SessionUpdate(_ context.Context, n libacp.SessionNotification) error {
	c.updates <- n
	return nil
}

type fwdACPHarness struct {
	client    *libacp.ClientSideConnection
	lc        *fwdACPClient
	stderrBuf *fwdLockedBuffer
}

func (h *fwdACPHarness) stderr() string { return h.stderrBuf.String() }

// newSessionCommands opens a fresh session and returns its id and the advertised
// slash-command names (from the available_commands_update the agent emits after
// the session/new result).
func (h *fwdACPHarness) newSessionCommands(t *testing.T, ctx context.Context, cwd string) (libacp.SessionID, []string) {
	t.Helper()
	resp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: cwd, McpServers: []libacp.McpServer{}})
	require.NoErrorf(t, err, "session/new failed\nacp stderr:\n%s", h.stderr())
	deadline := time.After(10 * time.Second)
	for {
		select {
		case n := <-h.lc.updates:
			if n.Update.SessionUpdate == libacp.SessionUpdateAvailableCommands {
				names := make([]string, 0, len(n.Update.AvailableCommands))
				for _, c := range n.Update.AvailableCommands {
					names = append(names, c.Name)
				}
				return resp.SessionID, names
			}
		case <-deadline:
			t.Fatalf("timed out waiting for available_commands_update\nacp stderr:\n%s", h.stderr())
		}
	}
}

// promptFor sends one prompt (here always a `/mission …` slash command, which
// acpsvc intercepts) and returns the text of the agent_message_chunk the command
// emits — the confirmation, or the teaching/command error rendered inline.
func (h *fwdACPHarness) promptFor(t *testing.T, ctx context.Context, sid libacp.SessionID, text string) string {
	t.Helper()
	_, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: sid,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent(text)},
	})
	require.NoErrorf(t, err, "prompt failed\nacp stderr:\n%s", h.stderr())
	deadline := time.After(15 * time.Second)
	for {
		select {
		case n := <-h.lc.updates:
			if n.Update.SessionUpdate == libacp.SessionUpdateAgentMessageChunk {
				if c := n.Update.Content; c != nil && strings.TrimSpace(c.Text) != "" {
					return c.Text
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the command's agent message\nacp stderr:\n%s", h.stderr())
		}
	}
}

// stdioRWC adapts a subprocess's stdout (read) + stdin (write) into the single
// ReadWriteCloser libacp speaks over — exactly how an editor talks to `contenox
// acp`, but with the pipes an exec.Cmd hands back instead of the editor's tty.
type stdioRWC struct {
	r io.Reader
	w io.WriteCloser
}

func (s stdioRWC) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s stdioRWC) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s stdioRWC) Close() error                { return s.w.Close() }

// fwdLockedBuffer is a concurrency-safe sink for a subprocess's stderr/log.
type fwdLockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *fwdLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *fwdLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ── self-contained build/CLI/port helpers (no dependency on sibling test files) ──

// fwdBuildBin compiles cmd/contenox into t.TempDir(); the go build cache makes
// reruns cheap.
func fwdBuildBin(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "contenox")
	out, err := exec.Command("go", "build", "-o", binPath, "github.com/contenox/beam/cmd/contenox").CombinedOutput()
	require.NoErrorf(t, err, "build contenox:\n%s", out)
	return binPath
}

// fwdRunCLI runs a contenox setup command and fails on non-zero exit.
func fwdRunCLI(t *testing.T, bin string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "contenox %v:\n%s", args, out)
}

// fwdFreePort returns a currently-free loopback TCP port. A small race window
// exists between close and serve's bind, acceptable for a test.
func fwdFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// inprocSeed declares the fired agent as an external `contenox acp --auto` unit
// bound to its chain (the child inherits the editor's $HOME/DB via os.Environ),
// and seeds the global config the in-process /mission handler reads. One db handle,
// closed before it returns, so the editor and its child open the SQLite file
// without contending with this seeding write.
func inprocSeed(t *testing.T, dbPath, bin, chainPath string) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	agent := &runtimetypes.Agent{Name: inprocAgentName, Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		// --auto: the unit runs its mission_report tool unattended (no HITL), and
		// the chain path marks it a DISPATCHED UNIT so it hosts no fleet of its own.
		Args: []string{"acp", "--auto"},
		Env:  map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agentregistryservice.New(db).Create(ctx, agent))

	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, clikv.WriteConfig(ctx, store, "", "default-mission-agent", inprocAgentName))
	require.NoError(t, clikv.WriteConfig(ctx, store, "", "default-mission-policy", "hitl-policy-default.json"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "", "update-check", "false"))
}

// inprocSeedConfig seeds only the global config (no agent) for the forwarding
// honesty test, where the dispatch never reaches a real agent.
func inprocSeedConfig(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, clikv.WriteConfig(ctx, store, "", "default-mission-agent", inprocAgentName))
	require.NoError(t, clikv.WriteConfig(ctx, store, "", "default-mission-policy", "hitl-policy-default.json"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "", "update-check", "false"))
}

// inprocSpawnACP starts `contenox acp` and drives it with a real libacp client
// over its stdio, returning the harness, the *exec.Cmd (so the test owns the
// process and can read its pid for the reaping assertion), and a `shutdown` that
// closes the editor's stdin — the Zed-disconnect path. Closing stdin is what
// actually shuts a stdio ACP process down: its conn.Run is blocked on an os.Stdin
// read that a mere SIGTERM (ctx cancel) cannot interrupt, so EOF is the reliable
// teardown trigger (the same reason fwdSpawnACP's cleanup closes stdin). Reuses
// the forwarding e2e's client harness types.
func inprocSpawnACP(t *testing.T, bin string, env []string) (*fwdACPHarness, *exec.Cmd, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.Command(bin, "acp")
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stderrBuf := &fwdLockedBuffer{}
	cmd.Stderr = stderrBuf
	require.NoError(t, cmd.Start())

	lc := &fwdACPClient{updates: make(chan libacp.SessionNotification, 256)}
	client := libacp.NewClientSideConnection(stdioRWC{r: stdout, w: stdin}, func(*libacp.ClientSideConnection) libacp.Client {
		return lc
	})
	go func() { _ = client.Run(ctx) }()

	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			cancel()
			_ = stdin.Close()
		})
	}

	t.Cleanup(func() {
		shutdown()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	return &fwdACPHarness{client: client, lc: lc, stderrBuf: stderrBuf}, cmd, shutdown
}

// inprocReportMeta is the _meta envelope the report router stamps on a delivered
// report (reportrouter.reportUpdateMeta's wire shape).
type inprocReportMeta struct {
	Report *struct {
		MissionID string `json:"missionId"`
		Kind      string `json:"kind"`
	} `json:"contenox.missionReport"`
}

// waitForMissionReport drains the client's update stream for the live report: an
// agent_message_chunk carrying both the summary text and the contenox.missionReport
// _meta — the routed report the firing session must see, recognizable as a mission
// report rather than as an ordinary agent message.
func waitForMissionReport(t *testing.T, h *fwdACPHarness, summary string) libacp.SessionNotification {
	t.Helper()
	deadline := time.After(90 * time.Second)
	for {
		select {
		case n := <-h.lc.updates:
			if n.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk || len(n.Update.Meta) == 0 {
				continue
			}
			var meta inprocReportMeta
			if json.Unmarshal(n.Update.Meta, &meta) != nil || meta.Report == nil {
				continue
			}
			if c := n.Update.Content; c != nil && strings.Contains(c.Text, summary) {
				return n
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the live mission-report update carrying contenox.missionReport _meta\nacp stderr:\n%s", h.stderr())
		}
	}
}

// waitProcess waits for cmd to exit within timeout, force-killing on overrun so a
// wedged editor cannot hang the test. This is the primary Wait; the spawn's
// cleanup Wait is then a harmless second call.
func waitProcess(t *testing.T, cmd *exec.Cmd, timeout time.Duration, h *fwdACPHarness) {
	t.Helper()
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("the acp editor did not exit within %s of stdin close\nacp stderr:\n%s", timeout, h.stderr())
	}
}

// childPIDs returns the pids of live processes whose parent is parentPid — the
// dispatched unit subprocess(es) an editor spawned. Linux-only (the e2e env),
// read straight from /proc.
func childPIDs(parentPid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, convErr := strconv.Atoi(e.Name())
		if convErr != nil {
			continue
		}
		if ppid, ok := procPPID(pid); ok && ppid == parentPid {
			out = append(out, pid)
		}
	}
	return out
}

// procPPID reads the parent pid from /proc/<pid>/stat. The comm field (field 2)
// can contain spaces and parentheses, so PPID is parsed from AFTER the last ')'.
func procPPID(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+1 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] = state, fields[1] = ppid.
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// pidAlive reports whether pid names a live process (signal 0 probe): ESRCH means
// gone (reaped), any other outcome means it still exists.
func pidAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
