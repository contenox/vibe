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

// closeTimeout bounds how long Handle.Close waits for the underlying
// ClientSideConnection's read loop (Run) to observe the transport closing
// and return, after the transport has been asked to close. It mirrors, one
// layer up, the grace-period pattern acpexec.Process.Close already applies
// to the subprocess itself.
const closeTimeout = 5 * time.Second

// ExternalACPAgent is the runtimetypes.AgentKindExternalACP implementation
// of Agent: it connects to an external ACP agent described by an
// ExternalACPConfig, either by spawning it as a subprocess over stdio (the
// v1, implemented path — it wraps libacp/acpexec, the same subprocess
// plumbing the client-e2e tests use to drive testy) or, in the future, by
// dialing it as a network endpoint (not implemented yet; Connect returns a
// clear error for that transport instead of silently doing nothing).
type ExternalACPAgent struct {
	Config runtimetypes.ExternalACPConfig

	// Stderr, if set, receives the spawned subprocess's stderr as it is
	// written (see acpexec.WithStderr). Defaults to io.Discard.
	Stderr io.Writer

	// KillGrace, if positive, overrides how long teardown waits for the
	// spawned agent to exit after its stdin is closed before killing it
	// (see acpexec.WithKillGrace; default 5s). Persistent agents — testy,
	// most editor adapters — never exit on stdin-close, so a short grace
	// here is what keeps their teardown from stalling for the full default.
	KillGrace time.Duration

	// Tracker observes the sandbox that confines the spawned agent (the
	// libsandbox.Command assembly lifecycle, and — in later libsandbox slices —
	// every blocked bypass attempt). It is an optional seam so a caller can wire
	// real telemetry later without this package requiring it: nil is treated as
	// libtracker.NoopTracker (see buildAgentCmd), so the default is silent.
	Tracker libtracker.ActivityTracker

	// SelfSpawn marks this not as a FOREIGN agent but as THIS runtime re-invoking
	// its own binary as an ACP unit (agentinstance.chainSpawner: `contenox acp`
	// bound to a chain file). Such a unit is spawned WITHOUT the sandbox, and that
	// is the whole reason the field exists.
	//
	// The wall confines code contenox did not write. A chain unit IS contenox: it
	// shares the one global runtime state — HOME resolves the database, the seeded
	// presets, the workspace id — which is precisely what distinguishes it from an
	// external agent that brings its own everything. Confining it would deny that
	// state on purpose: ~/.contenox is deliberately NOT carved (control-plane
	// isolation — an agent must never reach the policy that governs it), so a
	// confined unit cannot open the database it is supposed to share, and the
	// scrubbed env would strip the very inheritance it is defined by. What governs a
	// chain unit instead is in-process and stronger for this shape: its tool calls
	// run through the runtime's own capability grants and are gated by HITL over
	// session/request_permission, the same human-in-the-loop path an external
	// agent's requests take.
	//
	// It is therefore NOT an escape hatch for foreign agents: no config field and no
	// env toggle sets it. The kernel's chain spawn sets it explicitly; the same
	// exemption is otherwise INFERRED, and only from the one fact that cannot be
	// spoofed into a wider grant — the command resolving to this very executable
	// (see selfInvocation, which buildAgentCmd consults).
	SelfSpawn bool
}

// NewExternalACPAgent returns an ExternalACPAgent for cfg.
func NewExternalACPAgent(cfg runtimetypes.ExternalACPConfig) *ExternalACPAgent {
	return &ExternalACPAgent{Config: cfg}
}

var _ Agent = (*ExternalACPAgent)(nil)

// Connect validates a.Config and dispatches to the transport-specific
// connect path. harness is passed straight through to the
// libacp.ClientSideConnection this establishes — see Agent.Connect's doc
// comment for why that seam matters.
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
		// a.Config.Validate() above already rejects any transport other
		// than stdio/endpoint, so this is unreachable in practice — kept as
		// defense in depth against that invariant changing underneath us.
		return nil, fmt.Errorf("agenthost: unknown transport %q", a.Config.Transport)
	}
}

// connectStdio spawns a.Config.Command as a subprocess (acpexec.Spawn) and
// wires a libacp.ClientSideConnection to it over its stdin/stdout, exactly
// as libacp/acpexec's own client-e2e tests wire one to testy.
//
// ctx governs the spawned subprocess's entire lifetime, not just the connect
// step: acpexec.Spawn closes the process down (the same way Handle.Close
// would) the moment ctx is cancelled. A caller that wants a long-lived agent
// independent of whatever short-lived ctx it happened to call Connect with
// should pass one it controls directly (e.g. context.Background()) and rely
// on Handle.Close, not ctx cancellation, for teardown.
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

// sandboxDocsURL points an operator at the guide that explains what the agent
// sandbox needs and why an external agent is refused when the wall cannot be built
// on this host. Surfaced in the preflight error so a "your host can't confine"
// failure is actionable, not cryptic.
const sandboxDocsURL = "https://contenox.com/docs/guide/agent-sandbox/"

// sandboxCarveoutFile is the control-plane necessity-list an operator may place
// at ~/.contenox/sandbox-carveouts.json to widen the wall for a specific
// deployment: extra filesystem holes and the registry hosts the agent's
// toolchain must reach. Absent = the defaults only, offline by construction. It
// is read from the OPERATOR's real home (the parent process configuring the
// wall), never from the confined agent's reach — ~/.contenox is not carved, so
// the agent cannot see it.
const sandboxCarveoutFile = ".contenox/sandbox-carveouts.json"

// buildAgentCmd assembles the confined *exec.Cmd for spawning the external ACP
// agent described by a, through libsandbox: the agent is ALWAYS spawned inside
// "the wall" (a workspace-pinned, credential-scrubbed, offline-by-default
// sandbox), never as a bare subprocess. For a FOREIGN agent there is deliberately
// no unsandboxed path, so on a host where the wall cannot be built (non-Linux, or
// a Linux kernel without Landlock) libsandbox.Command fails closed and the agent
// does not run. The single exception is not a foreign agent at all — this runtime
// re-invoking its own binary as a unit (see SelfSpawn and selfInvocation) — and it
// is decided first, before any of the policy below applies.
//
// The Spec is fixed policy, not caller-tunable:
//   - WorkspaceRoot is a.Config.Cwd, the one writable root. An empty Cwd is a
//     fail-closed error: the wall needs a concrete workspace and must never
//     default to the whole filesystem.
//   - Home is the operator's REAL home. "~" in a carve-out resolves against it,
//     so "~/.claude" reaches the operator's actual agent config — while Landlock
//     denies everything not carved, so ~/.ssh, ~/.aws, ~/.contenox stay out of
//     reach even though the real home is $HOME. (A separate scoped home would
//     make "~/.claude" resolve to an empty dir and the agent would not find its
//     config.)
//   - EnvAllow is the resolved default env policy (PATH/TERM/locale, no secrets);
//     EnvSet is the agent config's explicit vars. HOME is forced to Home by
//     libsandbox regardless of either.
//   - FS carves the agent auth/config dirs read-only, plus anything the operator
//     added in the carve-out file. Net comes only from that file — empty means
//     offline. AllowPrivateEgress and SyscallTap are off.
func buildAgentCmd(ctx context.Context, a *ExternalACPAgent) (*exec.Cmd, error) {
	// The one exception, and it is not a foreign agent at all: this runtime
	// re-invoking its own binary — declared (the kernel's chain spawn) or merely
	// registered that way (a dispatched mission unit is `contenox acp --auto` bound
	// to a chain file, an ordinary external_acp record whose command happens to be
	// us). See SelfSpawn for why the wall is the wrong instrument there and what
	// governs such a unit instead.
	if a.SelfSpawn || selfInvocation(a.Config.Command) {
		return selfSpawnCmd(a), nil
	}
	// Fail closed, early, and legibly: external agents run ONLY inside the wall, so
	// if this host cannot build even the floor (Landlock), refuse before spawning
	// anything and point the operator at the guide — rather than letting the wall's
	// fail-closed contract surface later as an opaque child-side error. The default
	// wall needs only Landlock (no userns/privilege), so on a capable Linux host
	// this passes; where it does not, the agent is not run unconfined.
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

	// Layer on any operator-authored carve-outs from the control-plane file: extra
	// filesystem holes appended to the defaults, and the network hosts (the only
	// source of an egress route — no file means no net). An absent file is the
	// deny-by-construction default; a present-but-malformed file fails closed
	// rather than silently widening or narrowing the wall.
	if err := applyCarveoutFile(filepath.Join(home, sandboxCarveoutFile), &spec); err != nil {
		return nil, err
	}

	// The namespaced network wall (a routeless userns/netns + the per-host egress
	// that serves the Net carve-outs) is OPT-IN: it needs unprivileged user
	// namespaces the host may withhold (e.g. AppArmor-restricted Ubuntu 24.04), so
	// the default is the zero-privilege fence — Landlock confines the filesystem and
	// exec and the env is scrubbed, but the network is left open. Turn it on when the
	// operator asked to confine the network: either by naming hosts in the carve-out
	// file (Net carve-outs are only reachable THROUGH the netns that serves them, so
	// naming one implies it) or explicitly via CONTENOX_SANDBOX_NETWORK_WALL for a
	// fully-offline netns with no carve-outs. On a host without unprivileged userns
	// the opt-in fails closed (libsandbox.Command refuses) rather than run unconfined.
	spec.NetworkWall = len(spec.Net) > 0 || networkWallOptIn()

	return libsandbox.Command(ctx, spec, a.Config.Command, a.Config.Args...)
}

// selfInvocation reports whether command names THIS running executable — the
// signature of contenox spawning contenox (a dispatched mission unit, a chain
// unit), which is not a foreign agent and is not confined (see SelfSpawn).
//
// Identity is decided by os.SameFile, not by string equality: the same binary is
// reached under different paths (a symlink on PATH, a relative command, /proc's
// resolved path), and a wall exemption that a rename could dodge — or that a
// lookalike path could win — would be worth nothing. A bare command name is
// resolved through PATH first, exactly as exec would resolve it. Anything that
// cannot be resolved or stat'ed is NOT us: this fails toward confinement.
//
// What it does NOT grant is the interesting part. Being this binary buys only the
// right to run as this binary: whatever the unit then does — its tools, its file
// writes, its shell — runs through the runtime's own capability grants and the HITL
// gate. It is not a way to smuggle a foreign program past the wall, because the
// program IS contenox.
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

// selfSpawnCmd assembles the UNCONFINED command for a self-spawned unit: this
// binary, run with the parent's own environment plus the config's explicit vars,
// pinned to the config's cwd when it declares one. Deliberately no libsandbox: see
// SelfSpawn for why, and note that the two properties such a unit is defined by —
// the inherited environment (HOME resolves the shared database and the seeded
// presets) and its reach into that state — are exactly what the wall removes.
//
// Like libsandbox.Command it does NOT bind the command's lifetime to ctx: the
// process is owned and supervised by whatever runs it (acpexec.Spawn, which does
// bind to its own ctx), not by the assembly step.
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

// networkWallOptIn reports whether the operator opted into the namespaced network
// wall via the CONTENOX_SANDBOX_NETWORK_WALL environment toggle (a truthy value:
// "1" or "true"). Absent or unset, the default zero-privilege fence is used. See
// the opt-in comment in buildAgentCmd and Spec.NetworkWall.
func networkWallOptIn() bool {
	switch os.Getenv("CONTENOX_SANDBOX_NETWORK_WALL") {
	case "1", "true", "TRUE", "yes", "on":
		return true
	default:
		return false
	}
}

// defaultAgentCarveouts is the baseline read-only filesystem necessity list every
// confined agent gets: the auth/config directories the common agent CLIs read to
// start. "~" resolves against Spec.Home (the operator's real home). Missing dirs
// are harmless — libsandbox skips a carve-out path that does not exist.
func defaultAgentCarveouts() []libsandbox.FSCarveout {
	return []libsandbox.FSCarveout{
		{Path: "~/.claude", Mode: libsandbox.ModeRO, Needs: "agent auth/config"},
		{Path: "~/.codex", Mode: libsandbox.ModeRO, Needs: "agent auth/config"},
		{Path: "~/.config/goose", Mode: libsandbox.ModeRO, Needs: "agent auth/config"},
	}
}

// applyCarveoutFile appends the filesystem carve-outs and sets the network
// carve-outs from the necessity-list file at path (see LoadCarveouts) into spec.
// An absent file is the offline-by-construction default and not an error; a file
// that cannot be opened or parsed fails closed.
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
