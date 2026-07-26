//go:build linux

package libsandbox_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

// Egress-probe wiring. The probe (layer 2, the confined agent) resolves and
// connects through the parent's userspace stack; these mirror the unexported
// egress constants in the package (egressGatewayIP:egressDNSPort, and a host the
// parent can resolve to the test backend).
const (
	egressProbePortEnv = "CONTENOX_SANDBOX_EGRESS_PORT" // backend port for the allow probe
	egressProbeDNS     = "10.191.0.1:53"                // == egressGatewayIP:egressDNSPort
	egressAllowedHost  = "localhost"                    // parent resolves this to the loopback backend
	egressBlockedHost  = "blocked.example"              // never carved out

	exitEgressBreach = 13 // a deny path unexpectedly succeeded — the wall leaked
)

// runEgressProbe performs one egress action inside the wall and maps the outcome
// to an exit code. It is dispatched from runProbe (see shim_test.go).
func runEgressProbe(action string) int {
	switch action {
	case "egress-allow":
		port := os.Getenv(egressProbePortEnv)
		ip, err := probeResolve(egressAllowedHost)
		if err != nil {
			fmt.Fprintln(os.Stderr, "egress-allow resolve:", err)
			return exitDenied
		}
		if err := probeEcho(net.JoinHostPort(ip.String(), port)); err != nil {
			fmt.Fprintln(os.Stderr, "egress-allow echo:", err)
			return exitOther
		}
		return exitAllowed

	case "egress-dns-deny":
		if ip, err := probeResolve(egressBlockedHost); err == nil {
			fmt.Fprintln(os.Stderr, "egress-dns-deny UNEXPECTEDLY resolved:", ip)
			return exitEgressBreach
		}
		return exitDenied

	case "egress-connect-deny":
		// A literal address never handed out by the allow-listing DNS. The stack
		// must refuse it (RST), so the dial fails; a success is a breach.
		c, err := net.DialTimeout("tcp", "203.0.113.5:80", 5*time.Second)
		if err == nil {
			_ = c.Close()
			fmt.Fprintln(os.Stderr, "egress-connect-deny UNEXPECTEDLY connected")
			return exitEgressBreach
		}
		return exitDenied

	case "egress-guarded-connect":
		// The carve-out host DOES resolve at DNS (it is on the allow-list), so the
		// agent gets a synthetic address and connects to it. The SSRF guard runs at
		// the TCP layer in the parent: with AllowPrivateEgress off and the host
		// resolving inward (loopback), the parent RSTs the connect. So resolve must
		// SUCCEED but the connect must FAIL — a successful connect is a breach.
		ip, err := probeResolve(egressAllowedHost)
		if err != nil {
			fmt.Fprintln(os.Stderr, "egress-guarded-connect resolve:", err)
			return exitOther // the guard is at connect, not resolve; a resolve failure is a test-env problem
		}
		port := os.Getenv(egressProbePortEnv)
		c, derr := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), 5*time.Second)
		if derr == nil {
			_ = c.Close()
			fmt.Fprintln(os.Stderr, "egress-guarded-connect UNEXPECTEDLY connected past the SSRF guard")
			return exitEgressBreach
		}
		return exitDenied

	default:
		return exitOther
	}
}

// probeResolve sends a single A query straight to the sandbox resolver and
// returns the answer. It is a hand-built query (not net.Resolver) on purpose: it
// bypasses /etc/hosts and IP-literal short-circuits, so every lookup — even
// "localhost" — actually crosses the TUN to the stack's allow-listing DNS.
func probeResolve(host string) (net.IP, error) {
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return nil, err
	}
	q := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 0x7a7a, RecursionDesired: true},
		Questions: []dnsmessage.Question{
			{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		},
	}
	packed, err := q.Pack()
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		conn, derr := net.DialTimeout("udp", egressProbeDNS, 3*time.Second)
		if derr != nil {
			lastErr = derr
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, werr := conn.Write(packed); werr != nil {
			conn.Close()
			lastErr = werr
			continue
		}
		buf := make([]byte, 512)
		n, rerr := conn.Read(buf)
		conn.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		var resp dnsmessage.Message
		if uerr := resp.Unpack(buf[:n]); uerr != nil {
			lastErr = uerr
			continue
		}
		if resp.Header.RCode != dnsmessage.RCodeSuccess {
			return nil, fmt.Errorf("resolve %q: rcode %v", host, resp.Header.RCode)
		}
		for _, a := range resp.Answers {
			if ar, ok := a.Body.(*dnsmessage.AResource); ok {
				return net.IP(ar.A[:]), nil
			}
		}
		return nil, fmt.Errorf("resolve %q: no A record", host)
	}
	return nil, fmt.Errorf("resolve %q: %w", host, lastErr)
}

// probeEcho connects to addr through the stack, sends a token, and requires it
// back — proving a real end-to-end byte path to the allow-listed backend.
func probeEcho(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return err
	}
	buf := make([]byte, 4)
	if _, err := c.Read(buf); err != nil {
		return err
	}
	if string(buf) != "ping" {
		return fmt.Errorf("echo mismatch: %q", buf)
	}
	return nil
}

// recTracker is a thread-safe ActivityTracker that records every event so a test
// can assert which allows and denies the wall reported. The egress bridge logs
// from several goroutines (DNS server, per-connection), so it must be safe for
// concurrent Start/report.
type recTracker struct {
	mu     sync.Mutex
	events []recEvent
}

type recEvent struct {
	op, subj string
	host     string
	id       string // the id passed to reportChange (e.g. the tap's syscall path)
	kv       []any  // the kvArgs passed to Start (e.g. "syscall", name)
	allow    bool   // reportChange was called
	deny     bool   // reportErr was called
}

func hostFromKV(kv []any) string {
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok && k == "host" {
			if v, ok := kv[i+1].(string); ok {
				return v
			}
		}
	}
	return ""
}

func (t *recTracker) Start(_ context.Context, op, subj string, kv ...any) (func(error), func(string, any), func()) {
	host := hostFromKV(kv)
	kvCopy := append([]any(nil), kv...)
	record := func(id string, allow, deny bool) {
		t.mu.Lock()
		t.events = append(t.events, recEvent{op: op, subj: subj, host: host, id: id, kv: kvCopy, allow: allow, deny: deny})
		t.mu.Unlock()
	}
	return func(error) { record("", false, true) },
		func(id string, _ any) { record(id, true, false) },
		func() {}
}

func (t *recTracker) has(subj, host string, allow bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.events {
		if e.subj == subj && e.host == host && ((allow && e.allow) || (!allow && e.deny)) {
			return true
		}
	}
	return false
}

// TestIntegration_NetEgress drives the metered egress wall end to end. It starts
// a loopback echo backend in the parent, declares "localhost" as the sole network
// carve-out, and confines THIS test binary through the full seam (Landlock + the
// user/network namespaces + the TUN/netstack egress bridge). From inside the wall
// it proves:
//
//   - an allow-listed host resolves (to a synthetic address) and a TCP byte path
//     to it reaches the real backend — enforced egress works;
//   - a non-carve-out host fails to resolve (NXDOMAIN) — deny-by-default at DNS;
//   - a literal address never handed out is refused — deny-by-default at TCP.
//
// It then asserts the injected tracker recorded both an allow event (for the
// resolved host) and deny events (for the blocked name), with the host names —
// the "watch the wall" telemetry for the network surface.
func TestIntegration_NetEgress(t *testing.T) {
	if !landlockSupported() {
		t.Skip("landlock filesystem ABI unavailable on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("unprivileged user+network namespaces unavailable on this host")
	}
	if !egressTunSupported() {
		t.Skip("creating a TUN in an unprivileged user+network namespace is unavailable on this host")
	}

	backend, port := startEchoBackend(t)
	defer backend.Close()

	ws := t.TempDir()
	home := t.TempDir()
	tracker := &recTracker{}

	newSpec := func(action string) libsandbox.Spec {
		return libsandbox.Spec{
			WorkspaceRoot: ws,
			Home:          home,
			// Carve-outs only mean anything with the network wall on — the wall is
			// what these tests poke a metered hole in; without it the spec is refused.
			NetworkWall: true,
			// The sole carve-out. "localhost" is what the parent resolves the
			// forwarded connection to — the loopback backend — while the agent only
			// ever sees the synthetic address the DNS server mints for it.
			Net: []libsandbox.NetCarveout{{Host: egressAllowedHost, Needs: "test egress backend"}},
			// The backend is a loopback address, which the SSRF guard would refuse by
			// default; this test legitimately egresses to it, so it opts in.
			AllowPrivateEgress: true,
			Tracker:            tracker,
			EnvSet: map[string]string{
				probeEnv:           action,
				egressProbePortEnv: strconv.Itoa(port),
			},
		}
	}

	cases := []struct {
		name   string
		action string
		want   int
	}{
		{"allow-listed host resolves and connects", "egress-allow", exitAllowed},
		{"non-carve-out host fails to resolve", "egress-dns-deny", exitDenied},
		{"literal address is refused", "egress-connect-deny", exitDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel() // tears the egress bridge down after the run

			cmd, err := libsandbox.Command(ctx, newSpec(tc.action), "/proc/self/exe")
			require.NoError(t, err)
			require.Equal(t, tc.want, exitCode(cmd.Run()),
				"egress probe %q: unexpected outcome", tc.action)
		})
	}

	require.True(t, tracker.has("sandbox-egress", egressAllowedHost, true),
		"expected an ALLOW telemetry event for %q", egressAllowedHost)
	require.True(t, tracker.has("sandbox-egress", egressBlockedHost, false),
		"expected a DENY telemetry event for %q", egressBlockedHost)
}

// TestIntegration_NetEgress_SSRFGuardRefusesLoopback proves the SSRF guard is
// WIRED into the connect path, not just present as a helper: with the SAME
// loopback carve-out as the allow test but AllowPrivateEgress OFF (the default),
// the agent still resolves the host (DNS allow-list is unaffected) yet the
// parent-side guard RSTs the connect because the carve-out resolves inward. The
// probe therefore resolves-then-fails-to-connect (exitDenied), and the tracker
// records a DENY for the host on the egress surface. This is the regression that a
// carve-out cannot pivot the agent onto the host's own loopback/internal network.
func TestIntegration_NetEgress_SSRFGuardRefusesLoopback(t *testing.T) {
	if !landlockSupported() {
		t.Skip("landlock filesystem ABI unavailable on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("unprivileged user+network namespaces unavailable on this host")
	}
	if !egressTunSupported() {
		t.Skip("creating a TUN in an unprivileged user+network namespace is unavailable on this host")
	}

	backend, port := startEchoBackend(t)
	defer backend.Close()

	ws := t.TempDir()
	home := t.TempDir()
	tracker := &recTracker{}

	spec := libsandbox.Spec{
		WorkspaceRoot: ws,
		Home:          home,
		// A loopback carve-out requires the wall (it only means something with it on).
		NetworkWall: true,
		// A loopback carve-out, but AllowPrivateEgress is deliberately OFF (default).
		Net: []libsandbox.NetCarveout{{Host: egressAllowedHost, Needs: "test egress backend"}},
		// AllowPrivateEgress: false — the guard must refuse the inward connect.
		Tracker: tracker,
		EnvSet: map[string]string{
			probeEnv:           "egress-guarded-connect",
			egressProbePortEnv: strconv.Itoa(port),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, err := libsandbox.Command(ctx, spec, "/proc/self/exe")
	require.NoError(t, err)
	require.Equal(t, exitDenied, exitCode(cmd.Run()),
		"a loopback carve-out must be REFUSED at connect when AllowPrivateEgress is off")

	require.True(t, tracker.has("sandbox-egress", egressAllowedHost, false),
		"expected a DENY telemetry event for the SSRF-refused host %q", egressAllowedHost)
}

// TestIntegration_EgressAndSyscallTap_Compose is the committed regression for the
// combined fd-bookkeeping path (FIX 6): a spec with BOTH Net carve-outs AND
// SyscallTap wires TWO inherited control sockets — the egress slot, then the tap
// slot — and the parent receives two fds over them (the TUN, then the seccomp
// notify fd), each close-on-exec. That composition previously had no checked-in
// test. It asserts the egress connect SUCCEEDS end-to-end AND the exec is tapped,
// so neither mechanism's fd handoff clobbers the other's.
func TestIntegration_EgressAndSyscallTap_Compose(t *testing.T) {
	requireTapPreconditions(t) // landlock + userns/netns + seccomp user-notify
	if !egressTunSupported() {
		t.Skip("creating a TUN in an unprivileged user+network namespace is unavailable on this host")
	}

	backend, port := startEchoBackend(t)
	defer backend.Close()

	ws := t.TempDir()
	home := t.TempDir()
	tracker := &recTracker{}

	spec := libsandbox.Spec{
		WorkspaceRoot:      ws,
		Home:               home,
		NetworkWall:        true, // carve-outs require the wall
		Net:                []libsandbox.NetCarveout{{Host: egressAllowedHost, Needs: "test egress backend"}},
		AllowPrivateEgress: true, // the backend is a loopback address
		SyscallTap:         true,
		Tracker:            tracker,
		EnvSet: map[string]string{
			probeEnv:           "egress-allow",
			egressProbePortEnv: strconv.Itoa(port),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, err := libsandbox.Command(ctx, spec, "/proc/self/exe")
	require.NoError(t, err)
	require.Equal(t, exitAllowed, exitCode(cmd.Run()),
		"with egress + tap composed, the allow-listed egress connect must still succeed")

	require.True(t, tracker.has("sandbox-egress", egressAllowedHost, true),
		"egress ALLOW telemetry must be recorded alongside the tap")
	require.True(t, tracker.hasSyscall("execve", ""),
		"the tap must record the execve even while the egress bridge is also wired")
}

// startEchoBackend runs a loopback TCP echo server for the duration of the test
// and returns it with its port. It stands in for the "real host" an allow-listed
// carve-out names: the parent-side forwarder dials it when the agent connects to
// the synthetic address for egressAllowedHost.
func startEchoBackend(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 256)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						_, _ = conn.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr).Port
}

// egressTunSupported reports whether this host lets an unprivileged process create
// a TUN inside a user+network namespace — the extra precondition egress adds over
// the netns floor. It re-execs this binary into the exact clone the sandbox uses
// and has the child attempt the TUN creation the shim would, exiting 0 only if it
// worked; a kernel or policy that forbids it makes Run fail, and the egress test
// skips rather than failing on a host that structurally cannot do egress.
func egressTunSupported() bool {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return false
	}
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), egressTunCheckEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	return cmd.Run() == nil
}

const egressTunCheckEnv = "CONTENOX_SANDBOX_EGRESS_TUN_CHECK"

// egressTunCheckChild runs inside the clone (dispatched from TestMain) and exits 0
// iff a TUN can be created and configured in the fresh netns, mirroring the shim's
// createEgressTun without depending on package internals.
func egressTunCheckChild() {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		os.Exit(1)
	}
	ifr, err := unix.NewIfreq("ctxsbx0")
	if err != nil {
		os.Exit(1)
	}
	ifr.SetUint16(uint16(unix.IFF_TUN | unix.IFF_NO_PI))
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
