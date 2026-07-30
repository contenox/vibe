// Package modeldprobe detects whether the modeld daemon (the separate CGO
// inference binary) is installed, running, or dead, so the runtime can fail
// honestly and the setup wizard can guide the user.
//
// It is pure Go and self-contained: it locates the binary and inspects the
// owner lease via liblease. It deliberately does not import modeld — the only
// shared facts are the lease file name and the endpoint metadata key, mirrored
// here as constants (see cmd/modeld and modeld/owner.EndpointMetaKey).
//
// Detect() reports lease-based liveness, a network-free proxy: a wedged process
// can still hold a fresh lease. Probe() adds the real reachability ping over the
// runtime's gRPC transport — it confirms the lease holder actually answers and
// is the instance serving the endpoint, downgrading a wedged owner to
// unreachable. Probe imports the runtime transport client (not modeld). See
// docs/development/blueprints/modeld/provisioning-detection.md.
package modeldprobe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/contenox/contenox/liblease"
	"github.com/contenox/contenox/internal/models/modeldinstall"
	transportgrpc "github.com/contenox/contenox/libtransport/grpc"
)

// Convention shared with the daemon; kept local so runtime does not depend on
// the modeld package.
const (
	leaseFileName   = "modeld.lease" // mirrors cmd/modeld resolvePaths
	endpointMetaKey = "endpoint"     // mirrors modeld/owner.EndpointMetaKey
	backendMetaKey  = "backend"      // mirrors modeld/owner.BackendMetaKey
	binaryName      = "modeld"
	binaryEnv       = "CONTENOX_MODELD_BIN" // explicit override (VS Code/extension installs)
	dataRootEnv     = "CONTENOX_DATA_ROOT"
)

// State is the detected condition of the modeld daemon.
type State int

const (
	// StateNotInstalled means no modeld binary could be located.
	StateNotInstalled State = iota
	// StateNotRunning means the binary exists but no owner lease is present.
	StateNotRunning
	// StateStale means an owner lease exists but has expired: the daemon
	// likely crashed or was killed.
	StateStale
	// StateRunning means a live owner holds a fresh lease.
	StateRunning
	// StateUnreachable means a fresh lease names a live owner, but its advertised
	// endpoint does not answer a health probe: the daemon is wedged, still
	// bringing up its transport, or the advertised endpoint is wrong. Only Probe
	// (not Detect) can report this, because it requires a network round-trip.
	StateUnreachable
)

func (s State) String() string {
	switch s {
	case StateNotInstalled:
		return "not-installed"
	case StateNotRunning:
		return "not-running"
	case StateStale:
		return "stale"
	case StateRunning:
		return "running"
	case StateUnreachable:
		return "unreachable"
	default:
		return "unknown"
	}
}

// Typed, actionable errors. Status.Err returns one of these (or nil) so callers
// can branch with errors.Is.
var (
	ErrNotInstalled = errors.New("modeld is not installed")
	ErrNotRunning   = errors.New("modeld is not running")
	ErrStale        = errors.New("modeld owner is stale (the daemon may have crashed)")
	ErrUnreachable  = errors.New("modeld holds a lease but does not answer a health probe")
)

// Status is the result of a detection.
type Status struct {
	State    State
	Binary   string // resolved binary path, when located
	Endpoint string // advertised IPC endpoint, when running
	Instance string // owner instance UUID, when a lease is present
	Backend  string // inference backend the owner serves ("llama"/"openvino"/"none"), when running
}

// Err maps the status to a typed error, or nil when the daemon is running. A nil
// error means install + liveness passed; it does NOT by itself mean inference
// works, because the wire transport is a separate concern.
func (s Status) Err() error {
	switch s.State {
	case StateNotInstalled:
		return ErrNotInstalled
	case StateNotRunning:
		return ErrNotRunning
	case StateStale:
		return ErrStale
	case StateUnreachable:
		return ErrUnreachable
	default:
		return nil
	}
}

// Detector resolves the modeld state. Construct it with New; the now and
// lookPath fields are injectable for tests.
type Detector struct {
	leasePath      string
	binaryOverride string
	dataRoot       string
	lookPath       func(string) (string, error)
	statBinary     func(string) bool
	findManaged    func(context.Context, string) (string, error)
	now            func() time.Time
	// health performs the reachability ping; injectable for tests. nil disables
	// the ping (Probe then behaves like Detect).
	health func(ctx context.Context, endpoint, expectedInstance string) error
}

// New returns a Detector for the given data root (where the owner lease lives).
// An empty dataRoot falls back to DefaultDataRoot.
func New(dataRoot string) *Detector {
	if dataRoot == "" {
		dataRoot = DefaultDataRoot()
	}
	return &Detector{
		leasePath:      filepath.Join(dataRoot, leaseFileName),
		binaryOverride: os.Getenv(binaryEnv),
		dataRoot:       dataRoot,
		lookPath:       exec.LookPath,
		statBinary:     fileExists,
		findManaged:    findManagedModeldBinary,
		now:            time.Now,
		health:         grpcHealthCheck,
	}
}

func findManagedModeldBinary(ctx context.Context, dataRoot string) (string, error) {
	inst, err := modeldinstall.FindCompatibleInstall(ctx, dataRoot, runtime.GOOS, runtime.GOARCH, "")
	if err != nil {
		return "", err
	}
	return inst.LauncherPath, nil
}

// Probe resolves the state and, when a fresh lease names a live owner, confirms
// the owner actually answers a health ping on its advertised endpoint. A lease
// is only a liveness proxy; this downgrades a wedged owner (fresh lease, dead
// transport) or a stale/mismatched endpoint from running to unreachable. Pass a
// ctx with a deadline — the ping dials the network. Detect() remains the cheap,
// network-free check for callers that only need lease state.
func (d *Detector) Probe(ctx context.Context) Status {
	st := d.Detect()
	if st.State != StateRunning || d.health == nil {
		return st
	}
	if st.Endpoint == "" || d.health(ctx, st.Endpoint, st.Instance) != nil {
		st.State = StateUnreachable
	}
	return st
}

// grpcHealthCheck dials the advertised endpoint and pings the owner. It confirms
// the process answering is the lease holder (instance match) and is ready, so a
// takeover-in-progress or a stale endpoint reads as unreachable rather than
// healthy. It is not fenced: liveness must be observable without owning a token.
func grpcHealthCheck(ctx context.Context, endpoint, expectedInstance string) error {
	c, err := transportgrpc.DialLeader(endpoint, "")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	h, err := c.Health(ctx)
	if err != nil {
		return err
	}
	if !h.Ready {
		return errors.New("modeld reports not ready")
	}
	if expectedInstance != "" && h.InstanceID != expectedInstance {
		return fmt.Errorf("owner mismatch: lease=%s serving=%s", expectedInstance, h.InstanceID)
	}
	return nil
}

// Detect resolves the current modeld state. A live lease takes precedence (the
// daemon is running regardless of whether we can locate the binary locally,
// e.g. when an extension started it from its own directory); binary presence
// then distinguishes stale/not-running from not-installed.
func (d *Detector) Detect() Status {
	rec, leaseErr := liblease.Inspect(d.leasePath)
	hasLease := leaseErr == nil
	live := hasLease && d.now().Before(rec.ExpiresAt())

	binary := d.locate()

	switch {
	case live:
		return Status{
			State:    StateRunning,
			Binary:   binary,
			Endpoint: rec.Meta[endpointMetaKey],
			Instance: rec.InstanceID,
			Backend:  rec.Meta[backendMetaKey],
		}
	case binary == "":
		return Status{State: StateNotInstalled}
	case hasLease: // present but expired
		return Status{State: StateStale, Binary: binary, Instance: rec.InstanceID}
	default:
		return Status{State: StateNotRunning, Binary: binary}
	}
}

// locate resolves the modeld binary: an explicit override first (required for
// installs that are not on PATH, like the VS Code extension dir), then a
// protocol-compatible Contenox-managed install, then PATH.
func (d *Detector) locate() string {
	if d.binaryOverride != "" {
		if d.statBinary(d.binaryOverride) {
			return d.binaryOverride
		}
		return ""
	}
	if d.findManaged != nil {
		if p, err := d.findManaged(context.Background(), d.dataRoot); err == nil && p != "" && d.statBinary(p) {
			return p
		}
	}
	if p, err := d.lookPath(binaryName); err == nil {
		return p
	}
	return ""
}

// DefaultDataRoot resolves the contenox data root: $CONTENOX_DATA_ROOT, else
// ~/.contenox.
func DefaultDataRoot() string {
	if r := os.Getenv(dataRootEnv); r != "" {
		return r
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".contenox"
	}
	return filepath.Join(home, ".contenox")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
