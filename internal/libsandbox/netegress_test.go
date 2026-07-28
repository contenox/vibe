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

	"github.com/contenox/contenox/internal/libsandbox"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

// Egress-probe wiring; mirrors the unexported egress constants in the package
// (egressGatewayIP:egressDNSPort, and a host the parent resolves to the test
// backend).
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
		// A literal address never handed out by the allow-listing DNS; must be RST.
		c, err := net.DialTimeout("tcp", "203.0.113.5:80", 5*time.Second)
		if err == nil {
			_ = c.Close()
			fmt.Fprintln(os.Stderr, "egress-connect-deny UNEXPECTEDLY connected")
			return exitEgressBreach
		}
		return exitDenied

	case "egress-guarded-connect":
		// Host resolves (allow-listed), but the parent's SSRF guard RSTs the
		// connect since it resolves inward and AllowPrivateEgress is off: resolve
		// must succeed, connect must fail.
		ip, err := probeResolve(egressAllowedHost)
		if err != nil {
			fmt.Fprintln(os.Stderr, "egress-guarded-connect resolve:", err)
			return exitOther // guard is at connect, not resolve
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

// probeResolve sends a single A query straight to the sandbox resolver. A
// hand-built query, not net.Resolver, so it bypasses /etc/hosts and IP-literal
// short-circuits: even "localhost" crosses the TUN to the stack's DNS.
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

// probeEcho connects to addr, sends a token, and requires it back, proving a
// real end-to-end byte path to the allow-listed backend.
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

// recTracker is a thread-safe ActivityTracker recording every event so a test
// can assert which allows/denies the wall reported (the egress bridge logs
// from several goroutines).
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

// TestIntegration_NetEgress drives the metered egress wall end to end with
// "localhost" as the sole carve-out: an allow-listed host resolves and reaches
// the real backend, a non-carve-out host fails to resolve (deny-by-default at
// DNS), and a never-handed-out literal address is refused (deny-by-default at
// TCP). Also asserts the tracker recorded both an allow and a deny event.
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
			NetworkWall:   true, // carve-outs require the wall
			// Sole carve-out; the agent only sees the synthetic address minted for it.
			Net: []libsandbox.NetCarveout{{Host: egressAllowedHost, Needs: "test egress backend"}},
			// Backend is a loopback address, which the SSRF guard refuses by default.
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

// TestIntegration_NetEgress_SSRFGuardRefusesLoopback pins that the SSRF guard
// is wired into the connect path: with AllowPrivateEgress off (default), a
// loopback carve-out still resolves at DNS but the parent RSTs the connect,
// so a carve-out cannot pivot the agent onto the host's own network.
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
		NetworkWall:   true,
		Net:           []libsandbox.NetCarveout{{Host: egressAllowedHost, Needs: "test egress backend"}},
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

// TestIntegration_EgressAndSyscallTap_Compose pins the combined fd-bookkeeping
// path: a spec with both Net carve-outs and SyscallTap wires two inherited
// control sockets and receives two fds, each close-on-exec, without either
// mechanism's handoff clobbering the other's.
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

// startEchoBackend runs a loopback TCP echo server for the test's duration,
// standing in for the "real host" an allow-listed carve-out names.
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

// egressTunSupported reports whether this host lets an unprivileged process
// create a TUN inside a user+network namespace, by re-execing this binary
// into the exact clone the sandbox uses and attempting the TUN creation the
// shim would.
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

// egressTunCheckChild runs inside the clone (dispatched from TestMain) and
// exits 0 iff a TUN can be created, mirroring createEgressTun without
// depending on package internals.
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
