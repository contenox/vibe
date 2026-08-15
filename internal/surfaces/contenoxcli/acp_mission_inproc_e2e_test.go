package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/operatorinbox"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

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
	if runtime.GOOS != "linux" {
		t.Skip("child-process reaping is verified via /proc (see childPIDs); Linux-only")
	}

	bin := fwdBuildBin(t)

	root := t.TempDir()
	homeDir := filepath.Join(root, "home")
	workspaceDir := filepath.Join(root, "workspace")
	dataDir := filepath.Join(workspaceDir, ".contenox")
	dbPath := filepath.Join(homeDir, ".contenox", "local.db")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))

	baseEnv := append(os.Environ(),
		"HOME="+homeDir,
		"CONTENOX_DEFAULT_MODEL=inproc-e2e-fake-model",
		"CONTENOX_DEFAULT_PROVIDER=ollama",
		"CONTENOX_SERVER_URL=",
		"CONTENOX_SERVER_TOKEN=",
		"CONTENOX_ACP_CHAIN_PATH=",
	)

	fwdRunCLI(t, bin, baseEnv, "--data-dir", dataDir, "--db", dbPath, "init", "--force")

	chainsDir := filepath.Join(root, "chains")
	require.NoError(t, os.MkdirAll(chainsDir, 0o755))
	chainPath := filepath.Join(chainsDir, "inproc-reporter.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(inprocReporterChain), 0o644))

	inprocSeed(t, dbPath, bin, chainPath)

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	missions := missionservice.New(db)
	inbox := operatorinbox.New(db)

	h, cmd, shutdown := inprocSpawnACP(t, bin, baseEnv, workspaceDir)
	editorPID := cmd.Process.Pid

	_, err = h.client.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "zed", Version: "e2e"},
	})
	require.NoErrorf(t, err, "acp initialize failed\nacp stderr:\n%s", h.stderr())

	projectDir := filepath.Join(workspaceDir, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	sid, cmds := h.newSessionCommands(t, ctx, projectDir)
	require.Containsf(t, cmds, "mission",
		"/mission must be advertised by an editor that embeds the fleet in-process\nacp stderr:\n%s", h.stderr())

	confirmation := h.promptFor(t, ctx, sid, "/mission "+inprocAgentName+" "+inprocIntent)
	require.Contains(t, confirmation, "Mission fired", "the in-process fire is confirmed")
	require.Contains(t, confirmation, inprocAgentName, "the confirmation names the fired agent")
	require.Contains(t, confirmation, "live in this session",
		"the IN-PROCESS confirmation must promise live delivery into this session, not the inbox as the primary home")

	reportUpdate := waitForMissionReport(t, h, inprocReportText)
	require.Equal(t, sid, reportUpdate.SessionID,
		"the live report is delivered on the FIRING session the client knows")

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

	inboxItems, err := inbox.List(ctx, 100)
	require.NoError(t, err)
	for _, it := range inboxItems {
		require.NotEqualf(t, mission.ID, it.MissionID,
			"a report delivered live to its firing session must NOT also land in the operator inbox (mission %s)", mission.ID)
	}

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

type stdioRWC struct {
	r io.Reader
	w io.WriteCloser
}

func (s stdioRWC) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s stdioRWC) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s stdioRWC) Close() error                { return s.w.Close() }

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

func fwdBuildBin(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "contenox")
	out, err := exec.Command("go", "build", "-o", binPath, "github.com/contenox/contenox/cmd/contenox").CombinedOutput()
	require.NoErrorf(t, err, "build contenox:\n%s", out)
	return binPath
}

func fwdRunCLI(t *testing.T, bin string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "contenox %v:\n%s", args, out)
}

func fwdFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

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
		Args:      []string{"acp", "--auto"},
		Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agentregistryservice.New(db).Create(ctx, agent))

	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, clikv.WriteConfig(ctx, store, "", "default-mission-agent", inprocAgentName))
	require.NoError(t, clikv.WriteConfig(ctx, store, "", "default-mission-policy", "hitl-policy-default.json"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "", "update-check", "false"))
}

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

// inprocSpawnACP launches the agent in dir, the way an editor launches it in
// the project it has open. The launch directory is always the first workspace
// root (see workspace_roots.go), so a session rooted anywhere outside dir is
// refused — spawning with the test process's own cwd inherited would root the
// allowlist in the source tree.
func inprocSpawnACP(t *testing.T, bin string, env []string, dir string) (*fwdACPHarness, *exec.Cmd, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.Command(bin, "acp")
	cmd.Env = env
	cmd.Dir = dir
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

type inprocReportMeta struct {
	Report *struct {
		MissionID string `json:"missionId"`
		Kind      string `json:"kind"`
	} `json:"contenox.missionReport"`
}

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
