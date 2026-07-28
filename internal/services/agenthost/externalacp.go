package agenthost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/contenox/beam/libacp/acpexec"
)

// closeTimeout bounds how long Handle.Close waits for the read loop to return.
const closeTimeout = 5 * time.Second

// ExternalACPAgent is the Agent implementation for
// runtimetypes.AgentKindExternalACP; endpoint transport is not implemented yet.
type ExternalACPAgent struct {
	Config runtimetypes.ExternalACPConfig

	// Stderr, if set, receives the spawned subprocess's stderr. Defaults to io.Discard.
	Stderr io.Writer

	// KillGrace, if positive, overrides how long teardown waits after
	// stdin closes before killing the agent (default 5s).
	KillGrace time.Duration

	// Tracker observes the sandbox confining the spawned agent. Optional;
	// nil is treated as libtracker.NoopTracker.
	Tracker libtracker.ActivityTracker

	// SelfSpawn marks this as the runtime re-invoking its own binary, not a
	// foreign agent: it runs unsandboxed under the runtime's own capability
	// grants and HITL gate. Set by the chain spawn, or inferred from
	// selfInvocation — never by config or env.
	SelfSpawn bool
}

// NewExternalACPAgent returns an ExternalACPAgent for cfg.
func NewExternalACPAgent(cfg runtimetypes.ExternalACPConfig) *ExternalACPAgent {
	return &ExternalACPAgent{Config: cfg}
}

var _ Agent = (*ExternalACPAgent)(nil)

// Connect validates a.Config and dispatches to the transport-specific path.
func (a *ExternalACPAgent) Connect(ctx context.Context, harness libacp.Client) (*Handle, error) {
	if harness == nil {
		return nil, fmt.Errorf("agenthost: harness is required")
	}
	if err := a.Config.Validate(); err != nil {
		return nil, fmt.Errorf("agenthost: invalid external_acp config: %w", err)
	}

	switch a.Config.Transport {
	case runtimetypes.ExternalACPTransportStdio:
		return a.connectStdio(ctx, harness)
	case runtimetypes.ExternalACPTransportEndpoint:
		return nil, fmt.Errorf("agenthost: endpoint transport is not implemented yet (agent %q)", a.Config.URL)
	default:
		// Unreachable: Config.Validate rejects any other transport.
		return nil, fmt.Errorf("agenthost: unknown transport %q", a.Config.Transport)
	}
}

// connectStdio spawns a.Config.Command and wires a connection to it over
// stdin/stdout. ctx governs the whole subprocess lifetime (Spawn tears it
// down on cancellation), so a long-lived agent needs a ctx the caller owns.
func (a *ExternalACPAgent) connectStdio(ctx context.Context, harness libacp.Client) (*Handle, error) {
	cmd, err := buildAgentCmd(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("agenthost: sandbox external ACP agent %q: %w", a.Config.Command, err)
	}

	var opts []acpexec.Option
	if a.Stderr != nil {
		opts = append(opts, acpexec.WithStderr(a.Stderr))
	}
	if a.KillGrace > 0 {
		opts = append(opts, acpexec.WithKillGrace(a.KillGrace))
	}

	proc, err := acpexec.Spawn(ctx, cmd, opts...)
	if err != nil {
		return nil, fmt.Errorf("agenthost: spawn external ACP agent %q: %w", a.Config.Command, err)
	}

	conn := libacp.NewClientSideConnection(proc, func(*libacp.ClientSideConnection) libacp.Client {
		return harness
	})

	runDone := make(chan error, 1)
	go func() { runDone <- conn.Run(ctx) }()

	closeFn := func() error {
		procErr := proc.Close()
		select {
		case runErr := <-runDone:
			if procErr != nil {
				return procErr
			}
			return runErr
		case <-time.After(closeTimeout):
			if procErr != nil {
				return procErr
			}
			return fmt.Errorf("agenthost: ClientSideConnection.Run did not exit within %s of Close", closeTimeout)
		}
	}

	return &Handle{Conn: conn, closeFn: closeFn}, nil
}

// sandboxDocsURL is surfaced in the preflight error when the sandbox cannot be built.
const sandboxDocsURL = "https://contenox.com/docs/guide/agent-sandbox/"

// sandboxCarveoutFile is the operator-authored necessity-list at
// ~/.contenox/sandbox-carveouts.json that widens the wall. Absent means
// offline-by-construction defaults; read from the operator's real home.
const sandboxCarveoutFile = ".contenox/sandbox-carveouts.json"

// buildAgentCmd assembles the confined *exec.Cmd for agent a: every agent
// runs inside libsandbox's wall except a self-spawned unit (see SelfSpawn).
// Fails closed if the wall cannot be built. Home is always the operator's
// real home — Landlock still denies ~/.ssh, ~/.aws, ~/.contenox.
func buildAgentCmd(ctx context.Context, a *ExternalACPAgent) (*exec.Cmd, error) {
	// Self-spawn is exempt from the wall; see SelfSpawn.
	if a.SelfSpawn || selfInvocation(a.Config.Command) {
		return selfSpawnCmd(a), nil
	}
	// Fail closed if this host cannot build the Landlock floor.
	if err := libsandbox.Preflight(); err != nil {
		return nil, fmt.Errorf("external agents run only inside the sandbox, which cannot be built on this host: %w — see %s", err, sandboxDocsURL)
	}
	if a.Config.Cwd == "" {
		return nil, errors.New("cwd is required to confine the agent (the wall needs a workspace; it will not default to the whole filesystem)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve operator home for the sandbox: %w", err)
	}

	tracker := a.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}

	spec := libsandbox.Spec{
		WorkspaceRoot:      a.Config.Cwd,
		Home:               home,
		EnvAllow:           libsandbox.DefaultEnvPolicy().Resolve(os.Environ()),
		EnvSet:             a.Config.Env,
		FS:                 defaultAgentCarveouts(),
		AllowPrivateEgress: false,
		SyscallTap:         false,
		Tracker:            tracker,
	}

	// Layer operator carve-outs on top: absent file = defaults only; a
	// malformed one fails closed.
	if err := applyCarveoutFile(filepath.Join(home, sandboxCarveoutFile), &spec); err != nil {
		return nil, err
	}

	// The network wall is opt-in (needs unprivileged userns): on when Net
	// carve-outs are named or CONTENOX_SANDBOX_NETWORK_WALL is set;
	// otherwise Landlock still confines filesystem/exec but the network
	// stays open. Fails closed where userns is unsupported.
	spec.NetworkWall = len(spec.Net) > 0 || networkWallOptIn()

	return libsandbox.Command(ctx, spec, a.Config.Command, a.Config.Args...)
}

// selfInvocation reports whether command resolves to this running
// executable — contenox spawning contenox, not confined (see SelfSpawn).
// Identity is os.SameFile, not string equality; anything unresolved is
// treated as not us, failing toward confinement.
func selfInvocation(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	if !strings.ContainsRune(command, filepath.Separator) {
		resolved, lerr := exec.LookPath(command)
		if lerr != nil {
			return false
		}
		command = resolved
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return false
	}
	cmdInfo, err := os.Stat(command)
	if err != nil {
		return false
	}
	return os.SameFile(selfInfo, cmdInfo)
}

// selfSpawnCmd assembles the unconfined command for a self-spawned unit:
// this binary with the parent's environment plus the config's vars, pinned
// to Config.Cwd. Does not bind the command's lifetime to ctx — that is the
// caller's job.
func selfSpawnCmd(a *ExternalACPAgent) *exec.Cmd {
	cmd := exec.Command(a.Config.Command, a.Config.Args...)
	cmd.Dir = a.Config.Cwd
	env := os.Environ()
	for k, v := range a.Config.Env {
		env = append(env, k+"="+v) // last wins on exec, so this overrides an inherited name
	}
	cmd.Env = env
	return cmd
}

// networkWallOptIn reports whether CONTENOX_SANDBOX_NETWORK_WALL is set to a
// truthy value. See buildAgentCmd.
func networkWallOptIn() bool {
	switch os.Getenv("CONTENOX_SANDBOX_NETWORK_WALL") {
	case "1", "true", "TRUE", "yes", "on":
		return true
	default:
		return false
	}
}

// defaultAgentCarveouts lists the read-only auth/config dirs every confined
// agent gets; a missing dir is skipped, not an error.
func defaultAgentCarveouts() []libsandbox.FSCarveout {
	return []libsandbox.FSCarveout{
		{Path: "~/.claude", Mode: libsandbox.ModeRO, Needs: "agent auth/config"},
		{Path: "~/.codex", Mode: libsandbox.ModeRO, Needs: "agent auth/config"},
		{Path: "~/.config/goose", Mode: libsandbox.ModeRO, Needs: "agent auth/config"},
	}
}

// applyCarveoutFile loads carve-outs from path into spec; an absent file is
// not an error, a malformed one fails closed.
func applyCarveoutFile(path string, spec *libsandbox.Spec) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // absent file = defaults + no net
		}
		return fmt.Errorf("open sandbox carve-out file %q: %w", path, err)
	}
	defer f.Close()

	fs, net, err := libsandbox.LoadCarveouts(f)
	if err != nil {
		return fmt.Errorf("load sandbox carve-outs from %q: %w", path, err)
	}
	spec.FS = append(spec.FS, fs...)
	spec.Net = net
	return nil
}
