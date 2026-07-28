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

// TestSystem_ACPMissionInProcess asserts a standalone `contenox acp` editor with no serve advertises /mission, dispatches a real child subprocess, delivers its report live into the firing session (not the operator inbox), persists the mission in the shared db, and reaps the child when the editor exits.

// inprocReporterChain is the deterministic, model-free chain the dispatched
// child runs: files a result report, then a noop terminator, with no
// mission_finish, so it stays a live idle server until reaped.
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

	// CONTENOX_SERVER_URL and CONTENOX_ACP_CHAIN_PATH are both empty: no
	// forwarding, and this process is a top-level editor (not a dispatched
	// unit), so it hosts the fleet in-process.
	baseEnv := append(os.Environ(),
		"HOME="+homeDir,
		"CONTENOX_DEFAULT_MODEL=inproc-e2e-fake-model",
		"CONTENOX_DEFAULT_PROVIDER=ollama",
		"CONTENOX_SERVER_URL=",
		"CONTENOX_SERVER_TOKEN=",
		"CONTENOX_ACP_CHAIN_PATH=",
	)

	fwdRunCLI(t, bin, baseEnv, "--data-dir", dataDir, "--db", dbPath, "init", "--force")

	// The child's chain lives in a plain directory, not under .contenox, which
	// control-plane isolation refuses to discover from.
	chainsDir := filepath.Join(root, "chains")
	require.NoError(t, os.MkdirAll(chainsDir, 0o755))
	chainPath := filepath.Join(chainsDir, "inproc-reporter.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(inprocReporterChain), 0o644))

	inprocSeed(t, dbPath, bin, chainPath)

	// A handle to the shared store for assertions: the mission record lands
	// here; the report is delivered live rather than inboxed.
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
	// The report router reaches this session because it lives in this process,
	// so the update carrying contenox.missionReport _meta arrives at the
	// client, not the inbox.
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
	// The inbox is only the no-live-parent fallback.
	inboxItems, err := inbox.List(ctx, 100)
	require.NoError(t, err)
	for _, it := range inboxItems {
		require.NotEqualf(t, mission.ID, it.MissionID,
			"a report delivered live to its firing session must NOT also land in the operator inbox (mission %s)", mission.ID)
	}

	// ── Killing the editor reaps the child (no orphan) ───────────────────────────
	// Capture the dispatched unit's pid, shut the editor down via its stdin
	// (the Zed-disconnect path, which drives graceful teardown), and prove the
	// child does not outlive its parent.
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

// newSessionCommands opens a fresh session and returns its id and the
// advertised slash-command names.
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

// promptFor sends one prompt and returns the text of the agent_message_chunk
// the command emits.
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

// stdioRWC adapts a subprocess's stdout (read) + stdin (write) into the
// single ReadWriteCloser libacp speaks over.
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

// inprocSeed declares the fired agent as an external `contenox acp --auto`
// unit bound to its chain, and seeds the global config the in-process
// /mission handler reads. Closes its db handle before returning so the
// editor and its child don't contend with this seeding write.
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
		// --auto: the unit runs its mission_report tool unattended (no HITL); the
		// chain path marks it a dispatched unit hosting no fleet of its own.
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

// inprocSpawnACP starts `contenox acp` and drives it with a real libacp
// client over its stdio, returning the harness, the *exec.Cmd, and a
// `shutdown` that closes the editor's stdin — the only reliable teardown
// trigger, since conn.Run blocks on an os.Stdin read that SIGTERM can't
// interrupt.
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

// waitForMissionReport drains the client's update stream for the live
// report: an agent_message_chunk carrying both the summary text and the
// contenox.missionReport _meta.
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
