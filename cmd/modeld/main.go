// Command modeld is the contenox model daemon: the per-user, per-data-root
// owner of resident model state. It claims a cross-platform lease, renews it,
// and while it holds the lease it serves the runtime/transport.Service contract
// over gRPC (fenced by the owner instance id) so the runtime can open sessions
// and probe health. The served inference backend is selected at build time
// (-tags 'openvino openvino_genai' or -tags llamanode); a build with no local
// backend still owns the lease and answers health probes.
//
// Usage:
//
//	modeld serve   [--data-root DIR] [--ttl DURATION] [--listen ADDR] [--models-dir DIR] [--mem-max 8GiB] [--mem-reserve 2GiB] [--mem-cold 16GiB] [--min-hot-context 4096]
//	modeld status  [--data-root DIR] [--json]
//	modeld version [--json]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/contenox/contenox/liblease"
	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/modeld/devicelease"
	"github.com/contenox/contenox/internal/modeld/owner"
	"github.com/contenox/contenox/internal/modeld/slot"
	transportgrpc "github.com/contenox/contenox/internal/transport/grpc"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "modeld:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("modeld "+cmd, flag.ContinueOnError)
	dataRoot := fs.String("data-root", "", "contenox data root (default ~/.contenox)")
	ttl := fs.Duration("ttl", 30*time.Second, "lease duration; renewed at ttl/3")
	listen := fs.String("listen", "127.0.0.1:0", "gRPC listen address for serve")
	modelsDir := fs.String("models-dir", "", "directory of on-disk models (default <data-root>/models); resolves a session request's model when it omits an explicit path — the only way a remote node can locate a model, since a caller-supplied filesystem path is meaningless off the local machine")
	memMax := fs.String("mem-max", "", "maximum modeld resident memory budget (bytes or e.g. 8GiB)")
	memReserve := fs.String("mem-reserve", "", "memory to leave free for desktop/other workloads (bytes or e.g. 2GiB)")
	memCold := fs.String("mem-cold", "", "host-RAM KV cold-store budget (bytes or e.g. 16GiB)")
	minHotContext := fs.Int("min-hot-context", -1, "minimum hot context tokens to prefer before reducing llama GPU layers (unset uses default, 0 disables)")
	idleTTL := fs.String("idle-ttl", "", "release the resident model after it sits idle this long, freeing device memory (e.g. 5m; 0/off disables; default 5m)")
	asJSON := fs.Bool("json", false, "machine-readable JSON output (status)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// version needs neither a data root nor the lease: it reports build metadata
	// (compiled backends) so a release smoke check can run against a fresh binary.
	if cmd == "version" {
		return printVersion(*asJSON)
	}

	resolvedRoot, leasePath, err := resolvePaths(*dataRoot)
	if err != nil {
		return err
	}

	switch cmd {
	case "serve":
		policy, err := resolvePolicy(resolvedRoot, *memMax, *memReserve, *memCold, *minHotContext)
		if err != nil {
			return err
		}
		idle, err := resolveIdleTTL(resolvedRoot, *idleTTL)
		if err != nil {
			return err
		}
		return serve(resolvedRoot, leasePath, *ttl, *listen, *modelsDir, policy, idle)
	case "status":
		return status(leasePath, *asJSON)
	default:
		return fmt.Errorf("unknown command %q (want: serve | status | version)", cmd)
	}
}

func resolvePolicy(dataRoot, memMax, memReserve, memCold string, minHotContext int) (capacity.Policy, error) {
	policy := capacity.LoadPolicy(dataRoot)
	if memMax != "" {
		v, err := capacity.ParseBytes(memMax)
		if err != nil {
			return capacity.Policy{}, fmt.Errorf("parse --mem-max: %w", err)
		}
		policy.MaxResidentBytes = v
	}
	if memReserve != "" {
		v, err := capacity.ParseBytes(memReserve)
		if err != nil {
			return capacity.Policy{}, fmt.Errorf("parse --mem-reserve: %w", err)
		}
		policy.MinFreeBytes = v
	}
	if memCold != "" {
		v, err := capacity.ParseBytes(memCold)
		if err != nil {
			return capacity.Policy{}, fmt.Errorf("parse --mem-cold: %w", err)
		}
		policy.HostColdBudgetBytes = v
	}
	if minHotContext >= 0 {
		policy.MinHotContextTokens = minHotContext
	}
	return policy, nil
}

// defaultIdleTTL is the launch default for releasing an idle resident model. It
// matches the runtime warm-cache idle TTL so the two tiers agree by default.
const defaultIdleTTL = 5 * time.Minute

// resolveIdleTTL picks the idle-release TTL with precedence flag > env >
// modeld.json > default. "0" or "off" (any source) disables idle reaping. modeld
// is safe with zero configuration: the default keeps it from holding device
// memory indefinitely when left running idle.
func resolveIdleTTL(dataRoot, flagVal string) (time.Duration, error) {
	raw := strings.TrimSpace(flagVal)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("CONTENOX_MODELD_IDLE_TTL"))
	}
	if raw == "" {
		raw = idleTTLFromJSON(dataRoot)
	}
	if raw == "" {
		return defaultIdleTTL, nil
	}
	if raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "none") {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse --idle-ttl %q: %w", raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("parse --idle-ttl %q: must not be negative", raw)
	}
	return d, nil
}

// idleTTLFromJSON reads {"idle":{"ttl":"5m"}} from <dataRoot>/modeld.json. A
// missing file or field returns "" so the caller falls back to the default.
func idleTTLFromJSON(dataRoot string) string {
	if dataRoot == "" {
		return ""
	}
	path := filepath.Join(dataRoot, "modeld.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var raw struct {
		Idle struct {
			TTL string `json:"ttl"`
		} `json:"idle"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		fmt.Fprintf(os.Stderr, "modeld: %s is malformed, idle TTL falls back to the default: %v\n", path, err)
	}
	return strings.TrimSpace(raw.Idle.TTL)
}

// isLoopbackEndpoint reports whether host is a loopback address or "localhost".
// An empty host (e.g. ":8080", meaning "all interfaces") is NOT loopback.
func isLoopbackEndpoint(endpoint string) bool {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func resolvePaths(dataRoot string) (string, string, error) {
	if dataRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("resolve home: %w", err)
		}
		dataRoot = filepath.Join(home, ".contenox")
	}
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		return "", "", fmt.Errorf("create data root %q: %w", dataRoot, err)
	}
	return dataRoot, filepath.Join(dataRoot, "modeld.lease"), nil
}

func serve(dataRoot, leasePath string, ttl time.Duration, listen, modelsDir string, policy capacity.Policy, idleTTL time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "modeld starting: data_root=%q lease=%q listen=%q ttl=%s idle_ttl=%s memory={%s}\n",
		dataRoot, leasePath, listen, ttl, formatIdleTTL(idleTTL), formatPolicy(policy))
	logRuntimeEnv()

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", listen, err)
	}
	endpoint := lis.Addr().String()
	fmt.Fprintf(os.Stderr, "modeld listener ready: requested=%q endpoint=%q\n", listen, endpoint)
	if !isLoopbackEndpoint(endpoint) {
		fmt.Fprintf(os.Stderr, "modeld WARNING: listening on a non-loopback endpoint (%s) with no authentication or transport encryption. Anyone who can reach this address can open sessions and run inference. Only bind non-loopback on a trusted network (tailnet/VPN/VPC).\n", endpoint)
	}

	// Resolve the served backend before joining so the lease advertises the mode
	// (llama / openvino / none); the runtime's detector reads it without a
	// network round-trip. A build with no local backend still owns the lease and
	// answers health probes so detection reports the daemon running.
	svc, backend := selectBackend(policy)
	svc = devicelease.New(svc,
		devicelease.WithTTL(ttl),
		devicelease.WithMeta(map[string]string{
			"data_root": dataRoot,
			"backend":   backend,
		}),
	)

	o, err := owner.Join(ctx, owner.Config{LeasePath: leasePath, TTL: ttl, Endpoint: endpoint, Backend: backend})
	if err != nil {
		_ = lis.Close()
		return err
	}
	if !o.IsOwner() {
		_ = lis.Close()
		h := o.Holder()
		expiresAt := h.ExpiresAt()
		fmt.Printf("modeld follower: reason=lease_held data_root=%q holder_instance=%s holder_pid=%d holder_host=%s endpoint=%s backend=%s expires=%s ttl_left=%s\n",
			dataRoot,
			h.InstanceID,
			h.PID,
			h.Host,
			h.Meta[owner.EndpointMetaKey],
			h.Meta[owner.BackendMetaKey],
			expiresAt.Format(time.RFC3339),
			time.Until(expiresAt).Round(time.Second),
		)
		return nil
	}
	fmt.Printf("modeld owner started: instance=%s pid=%d data_root=%s endpoint=%s backend=%s ttl=%s\n",
		o.InstanceID(), os.Getpid(), dataRoot, endpoint, backend, ttl)

	// Serve the runtime/transport.Service contract over gRPC, fenced by the owner
	// instance id, while we hold the lease.
	slotSvc := slot.New(svc, slot.WithOwner(o.InstanceID()), slot.WithBackend(backend), slot.WithIdleTTL(idleTTL), slot.WithDataRoot(dataRoot), slot.WithModelsDir(modelsDir))
	svc = slotSvc
	fmt.Printf("modeld transport serving: instance=%s endpoint=%s backend=%s idle_ttl=%s\n", o.InstanceID(), endpoint, backend, formatIdleTTL(idleTTL))
	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()
	// Release a resident-but-idle model so device memory is freed and the GPU can
	// return to idle while the daemon keeps holding the lease. Stops with serveCtx.
	slotSvc.StartReaper(serveCtx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- transportgrpc.Serve(serveCtx, lis, svc, o.InstanceID(), backend) }()

	select {
	case <-ctx.Done(): // signal -> graceful shutdown
	case <-o.Lost(): // self-fenced: lost the lease, stop touching state
		fmt.Fprintln(os.Stderr, "modeld: lost lease, shutting down:", o.LostErr())
	case err := <-serveErr: // the transport server stopped on its own
		if err != nil {
			return fmt.Errorf("transport server: %w", err)
		}
	}

	// Stop serving (GracefulStop closes the listener), then release the lease so a
	// successor can take over immediately rather than waiting out the TTL.
	serveCancel()
	if err := o.Release(); err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	fmt.Printf("modeld owner stopped: instance=%s backend=%s\n", o.InstanceID(), backend)
	return nil
}

func status(leasePath string, asJSON bool) error {
	rec, err := liblease.Inspect(leasePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if asJSON {
				fmt.Println(`{"running":false}`)
			} else {
				fmt.Println("no modeld owner (no lease)")
			}
			return nil
		}
		return err
	}
	expired := time.Now().After(rec.ExpiresAt())
	if asJSON {
		out := struct {
			liblease.Record
			Expired bool `json:"expired"`
		}{Record: rec, Expired: expired}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	state := "valid"
	if expired {
		state = "expired (stale)"
	}
	expiresAt := rec.ExpiresAt()
	fmt.Printf("modeld lease: instance=%s pid=%d host=%s endpoint=%s backend=%s expires=%s ttl_left=%s [%s]\n",
		rec.InstanceID,
		rec.PID,
		rec.Host,
		rec.Meta[owner.EndpointMetaKey],
		rec.Meta[owner.BackendMetaKey],
		expiresAt.Format(time.RFC3339),
		time.Until(expiresAt).Round(time.Second),
		state,
	)
	return nil
}
