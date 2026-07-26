//go:build linux

package libsandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/contenox/beam/internal/libtracker"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// errEgressDenied marks a wall-hit on the network surface: a name that is not an
// egress carve-out, or a connection to an address that was never resolved for
// one. It is the value passed to reportErr so a deny is a first-class, greppable
// telemetry event, not a silent drop.
var errEgressDenied = errors.New("libsandbox: egress denied")

const egressNICID = 1

// egressBridge is the parent side of the network wall: a userspace TCP/IP stack
// (gVisor netstack) attached to the fd of the TUN the shim created in the agent's
// namespace. All of the agent's traffic is carried as bare IP over that fd, so it
// is transparent and not env-bypassable — there is no host route the agent can
// reach except through this stack, and this stack answers DNS only for carve-out
// hosts and dials only the addresses it resolved for them.
//
// One bridge serves one agent for the life of ctx. It is created (and its
// goroutine launched) by setupEgress before the process starts; it parks until
// the shim hands over the TUN fd, serves until ctx is done, then tears the stack
// down and closes the fd — which is also what lets the agent's now-idle network
// namespace be reclaimed.
type egressBridge struct {
	ctx          context.Context
	tracker      libtracker.ActivityTracker
	conn         *net.UnixConn // parent end of the shim control socket (fd transport + readiness)
	policy       *egressPolicy
	dialer       *net.Dialer
	allowPrivate bool // permit carve-outs that resolve to non-public IPs (Spec.AllowPrivateEgress)

	resolveMu sync.Mutex
	resolved  map[string]resolvedHost // carve-out host -> its resolved+vetted addresses (cached once)
}

// resolvedHost caches the one-time resolution of a carve-out host: the vetted IPs
// to dial, or the refusal/failure. Caching is what closes DNS rebinding — a later
// connection reuses this result rather than re-resolving, so the address the SSRF
// guard vetted is the address that is dialed.
type resolvedHost struct {
	ips []net.IP
	err error
}

// setupEgress wires the egress bridge for a command whose spec declared network
// carve-outs. It creates the parent↔shim control socket, hands the child end to
// the command as an inherited fd, launches the bridge goroutine, and returns the
// fd number the shim will find the socket at (to record in the plan). The bridge
// runs until ctx is done, so the caller scopes egress by scoping ctx.
//
// It appends exactly one entry to cmd.ExtraFiles; the returned fd number accounts
// for anything already there. Errors wrap ErrIsolation and leave the command
// unstarted — fail-closed, no half-wired egress.
func setupEgress(ctx context.Context, cmd *exec.Cmd, spec Spec, tracker libtracker.ActivityTracker) (int, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("%w: egress control socketpair: %v", ErrIsolation, err)
	}
	parentFD, childFD := pair[0], pair[1]

	// The child end goes to the command as an inherited fd. os/exec reads its Fd at
	// Start and the file's finalizer closes the parent's copy when the command is
	// dropped — so the bridge never touches it, which is what keeps its teardown
	// from racing cmd.Start()'s read of the same *os.File.
	childFile := os.NewFile(uintptr(childFD), "contenox-egress-shim")
	childFDNum := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, childFile)

	parentFile := os.NewFile(uintptr(parentFD), "contenox-egress-parent")
	conn, err := net.FileConn(parentFile)
	parentFile.Close() // net.FileConn dups the fd; drop our original
	if err != nil {
		return 0, fmt.Errorf("%w: egress control conn: %v", ErrIsolation, err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return 0, fmt.Errorf("%w: egress control conn is not unix", ErrIsolation)
	}

	b := &egressBridge{
		ctx:          ctx,
		tracker:      tracker,
		conn:         uc,
		policy:       newEgressPolicy(spec.Net),
		dialer:       &net.Dialer{Timeout: 15 * time.Second},
		allowPrivate: spec.AllowPrivateEgress,
	}
	go b.run()
	return childFDNum, nil
}

// run is the bridge's lifecycle: park for the TUN fd, attach the stack, signal
// readiness, serve until ctx is done, tear down. A watcher closes the control
// socket on ctx cancellation so a parked receive unblocks even if the process was
// never started.
func (b *egressBridge) run() {
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-b.ctx.Done():
			b.conn.Close() // unblock a parked ReadMsgUnix / drop the control socket
		case <-watchDone:
		}
	}()

	tunFD, err := b.recvTunFD()
	if err != nil {
		b.conn.Close()
		return
	}

	s, err := buildEgressStack(tunFD, b.handleTCP, b.handleDNS)
	if err != nil {
		unix.Close(tunFD)
		b.conn.Close()
		return
	}

	// Signal the shim that the stack is attached; it then proceeds to exec the
	// agent. A failed write means the shim is gone — tear down.
	if _, werr := b.conn.Write([]byte{'R'}); werr != nil {
		b.teardown(s, tunFD)
		return
	}

	<-b.ctx.Done()
	b.teardown(s, tunFD)
}

// teardown stops the stack and releases the device. s.Close stops the link
// dispatchers through gVisor's internal stop signal (not by closing the fd), so
// s.Wait returns without a read blocked on the fd; only then is the TUN fd closed
// — avoiding any window where a dispatcher reads a closed-or-reused descriptor.
// Closing the fd also lets the agent's now-idle network namespace be reclaimed.
func (b *egressBridge) teardown(s *stack.Stack, tunFD int) {
	s.Close()
	s.Wait()
	unix.Close(tunFD)
	b.conn.Close()
}

// recvTunFD blocks until the shim sends the TUN fd as SCM_RIGHTS ancillary data,
// then extracts it — close-on-exec, so it cannot leak into a later-spawned child.
func (b *egressBridge) recvTunFD() (int, error) {
	return recvOneFD(b.conn, "egress")
}

// recvOneFD receives exactly one fd sent as SCM_RIGHTS over conn, returning it
// already close-on-exec. The CLOEXEC is set ATOMICALLY at receipt via
// MSG_CMSG_CLOEXEC — not by a follow-up CloseOnExec — so there is no window in
// which the freshly received fd (agent A's live TUN or seccomp-notify fd) could
// be inherited by a concurrently exec'd sibling agent or local_shell and used to
// sniff/inject on A's egress or race A's telemetry. Passing the flag requires the
// raw fd (net.UnixConn.ReadMsgUnix has no flags argument), so this drops to
// unix.Recvmsg through conn.SyscallConn; readiness and ctx-cancellation still work
// through the runtime poller (a parked receive unblocks when conn is Closed by the
// lifecycle watcher). label names the surface for error strings; the one data byte
// is ignored — the fd is the payload.
func recvOneFD(conn *net.UnixConn, label string) (int, error) {
	data := make([]byte, 4)
	oob := make([]byte, unix.CmsgSpace(4)) // room for exactly one fd
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, err
	}
	var oobn int
	var opErr error
	if cerr := raw.Read(func(fd uintptr) bool {
		var e error
		_, oobn, _, _, e = unix.Recvmsg(int(fd), data, oob, unix.MSG_CMSG_CLOEXEC)
		if e == unix.EAGAIN || e == unix.EWOULDBLOCK || e == unix.EINTR {
			return false // not ready / interrupted — let the poller wait and retry
		}
		opErr = e
		return true
	}); cerr != nil {
		return -1, cerr
	}
	if opErr != nil {
		return -1, opErr
	}
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("parse control message: %w", err)
	}
	if len(scms) == 0 {
		return -1, fmt.Errorf("%s: no control message with fd", label)
	}
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		return -1, fmt.Errorf("parse fd rights: %w", err)
	}
	if len(fds) == 0 {
		return -1, fmt.Errorf("%s: control message carried no fd", label)
	}
	// MSG_CMSG_CLOEXEC already made the received fd close-on-exec at receipt, so
	// there is nothing more to seal and no separate-call leak window.
	return fds[0], nil
}

// handleDNS terminates the agent's DNS. Only :53 is served (all other UDP is left
// unhandled → the stack replies port-unreachable, so there is no non-DNS UDP
// egress). Each query is decided against the allow-list and logged: an allow is a
// reportChange against the host, a deny a reportErr — the "watch the wall"
// telemetry for the name-resolution surface.
func (b *egressBridge) handleDNS(r *udp.ForwarderRequest) bool {
	if r.ID().LocalPort != egressDNSPort {
		return false
	}
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		return true
	}
	go b.serveDNSQuery(gonet.NewUDPConn(&wq, ep))
	return true
}

func (b *egressBridge) serveDNSQuery(conn *gonet.UDPConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}
	resp, decision, ok := b.policy.answerDNS(buf[:n])
	if !ok {
		return
	}

	reportErr, reportChange, end := b.tracker.Start(b.ctx, "resolve", "sandbox-egress",
		"host", decision.Host, "type", decision.Type)
	switch {
	case decision.Err != nil:
		// Allow-listed but unresolvable here (synthetic range exhausted): a distinct,
		// logged failure, not a policy deny.
		reportErr(fmt.Errorf("resolve %q: %w", decision.Host, decision.Err))
	case decision.Allowed:
		data := map[string]any{"decision": "allow"}
		if decision.HasIP {
			data["synthetic_ip"] = net.IP(decision.IP[:]).String()
		}
		reportChange(decision.Host, data)
	default:
		reportErr(fmt.Errorf("%w: resolve %q refused (not a network carve-out)", errEgressDenied, decision.Host))
	}
	end()

	if resp != nil {
		_, _ = conn.Write(resp)
	}
}

// handleTCP terminates the agent's outbound TCP. The destination address is the
// synthetic token the DNS step handed out; it is mapped back to the allow-listed
// host, that host is resolved ONCE and SSRF-vetted, the real address is dialed
// from the parent's namespace, and bytes are piped. A destination that was never
// resolved (a literal IP the agent invented, or a name that was denied) maps to
// nothing and is refused with a RST; so is a port the carve-out did not open, and
// so is a host that resolves inward when AllowPrivateEgress is off. Every attempt
// is logged: allow via reportChange, deny/refused via reportErr.
func (b *egressBridge) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	var dst [4]byte
	copy(dst[:], id.LocalAddress.AsSlice())
	port := int(id.LocalPort)

	host, ok := b.policy.hostForSynth(dst)
	label := host
	if !ok {
		label = net.IP(dst[:]).String()
	}
	reportErr, reportChange, end := b.tracker.Start(b.ctx, "connect", "sandbox-egress",
		"host", label, "port", port)

	if !ok {
		reportErr(fmt.Errorf("%w: connect to %s:%d refused (not a resolved carve-out host)",
			errEgressDenied, net.IP(dst[:]).String(), port))
		r.Complete(true) // RST — the agent sees connection refused
		end()
		return
	}

	// Port granularity: a carve-out that named ports is reachable only on those.
	if !b.policy.portAllowed(host, port) {
		reportErr(fmt.Errorf("%w: connect to %s:%d refused (port not in the carve-out's allowed ports)",
			errEgressDenied, host, port))
		r.Complete(true)
		end()
		return
	}

	// SSRF guard: resolve the carve-out once (cached — no per-connection
	// re-resolution, which is what closes DNS rebinding) and refuse a host that
	// resolves to a loopback/link-local/private/unspecified/multicast address
	// unless AllowPrivateEgress opted in. This is the wall against a carve-out
	// pivoting the agent onto the host's own network (169.254.169.254, localhost
	// services, RFC1918 neighbours).
	vetted, rerr := b.resolveAndGuard(host)
	if rerr != nil {
		reportErr(rerr) // the deny/refusal, greppable telemetry
		r.Complete(true)
		end()
		return
	}

	backend, target, derr := b.dialVetted(vetted, port)
	if derr != nil {
		reportErr(fmt.Errorf("connect to carve-out %s (%s): %w", host, target, derr))
		r.Complete(true)
		end()
		return
	}

	var wq waiter.Queue
	ep, cerr := r.CreateEndpoint(&wq)
	if cerr != nil {
		backend.Close()
		reportErr(fmt.Errorf("accept egress to %s: %s", target, cerr))
		r.Complete(true)
		end()
		return
	}
	r.Complete(false)
	reportChange(host, map[string]any{"decision": "allow", "target": target})

	agent := gonet.NewTCPConn(&wq, ep)
	go func() {
		defer end()
		pipeConns(agent, backend)
	}()
}

// resolveAndGuard resolves a carve-out host exactly ONCE and applies the SSRF
// guard, caching the outcome (the vetted addresses or the refusal) for the life
// of the bridge. Caching is load-bearing security, not just a speed-up: because a
// later connection reuses the first result instead of re-resolving, a hostname
// that rebinds to an internal address between DNS answer and TCP connect cannot
// slip a fresh, unvetted address past the guard. First-writer wins under a race,
// so two simultaneous first connects still converge on one cached result.
func (b *egressBridge) resolveAndGuard(host string) ([]net.IP, error) {
	b.resolveMu.Lock()
	if r, ok := b.resolved[host]; ok {
		b.resolveMu.Unlock()
		return r.ips, r.err
	}
	b.resolveMu.Unlock()

	ips, err := b.lookupAndVet(host)

	b.resolveMu.Lock()
	defer b.resolveMu.Unlock()
	if r, ok := b.resolved[host]; ok {
		return r.ips, r.err // another goroutine resolved first; use its result
	}
	if b.resolved == nil {
		b.resolved = make(map[string]resolvedHost)
	}
	b.resolved[host] = resolvedHost{ips: ips, err: err}
	return ips, err
}

// lookupAndVet resolves host in the HOST namespace and keeps only the addresses
// safe to dial: when AllowPrivateEgress is off, every non-public class (loopback,
// link-local incl. cloud metadata, private, unspecified, multicast) is dropped.
// A host that resolves to nothing safe is refused as an egress deny. An IP literal
// carve-out is handled too — LookupIP returns it directly and it is vetted the
// same way.
func (b *egressBridge) lookupAndVet(host string) ([]net.IP, error) {
	ips, err := net.DefaultResolver.LookupIP(b.ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve carve-out %q: %w", host, err)
	}
	vetted := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if b.allowPrivate || isPublicEgressIP(ip) {
			vetted = append(vetted, ip)
		}
	}
	if len(vetted) == 0 {
		return nil, fmt.Errorf("%w: carve-out %q resolves only to non-public addresses "+
			"(loopback/link-local/private/unspecified/multicast); set AllowPrivateEgress "+
			"to permit an internal target", errEgressDenied, host)
	}
	return vetted, nil
}

// dialVetted dials the vetted addresses in order on port, returning the first
// connection that comes up (and the address string it reached). Trying each
// address preserves the dual-stack robustness the old hostname dial had — e.g. a
// host that resolves to both ::1 and 127.0.0.1 — without ever re-resolving, so the
// only addresses reachable are the ones the guard already vetted.
func (b *egressBridge) dialVetted(vetted []net.IP, port int) (net.Conn, string, error) {
	var lastErr error
	var lastTarget string
	for _, ip := range vetted {
		target := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		conn, err := b.dialer.DialContext(b.ctx, "tcp", target)
		if err == nil {
			return conn, target, nil
		}
		lastErr, lastTarget = err, target
	}
	return nil, lastTarget, lastErr
}

// pipeConns splices two connections until either direction ends, then closes both
// so the other copy unblocks. This is the whole data path once a connection is
// authorized: the netstack endpoint on the agent side, the real socket on the
// host side.
func pipeConns(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

// buildEgressStack stands up the userspace IPv4 stack over the TUN fd: an L3
// (no-Ethernet) link endpoint, a NIC in promiscuous + spoofing mode so it accepts
// and answers for the arbitrary destinations the agent targets, the gateway
// address, a catch-all route back out the NIC, and the TCP/UDP forwarders that
// hand new flows to the handlers. It owns nothing it does not create; the caller
// owns the TUN fd (fdbased does not close it).
//
// Ordering is load-bearing and is why the NIC is created disabled and enabled
// last: fdbased starts reading the fd — and delivering packets — the moment the
// NIC is attached, and a freshly-upped TUN already carries stray kernel traffic
// (IGMP/DAD). Registering the transport handlers before the NIC exists, then
// configuring the NIC while it is detached (Disabled), then enabling it last,
// means every piece of state a delivered packet touches is in place before the
// first packet can be delivered — no concurrent read/write of the handler slot or
// the NIC flags.
func buildEgressStack(tunFD int, tcpH func(*tcp.ForwarderRequest), udpH udp.ForwarderHandler) (*stack.Stack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	// Register the forwarders before any NIC exists, so the handler slot is set
	// before the dispatchers that read it can start.
	tf := tcp.NewForwarder(s, 0, 512, tcpH)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tf.HandlePacket)
	uf := udp.NewForwarder(s, udpH)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, uf.HandlePacket)

	link, err := fdbased.New(&fdbased.Options{
		FDs: []int{tunFD},
		MTU: egressMTU,
		// L3 TUN: packets are bare IP, no Ethernet framing (hence no ARP).
	})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("egress link endpoint: %v", err)
	}
	// Disabled: do not Attach yet — no fd reads, no packet delivery — while the NIC
	// is configured.
	if e := s.CreateNICWithOptions(egressNICID, link, stack.NICOptions{Disabled: true}); e != nil {
		s.Close()
		return nil, fmt.Errorf("egress nic: %s", e)
	}
	// The agent targets synthetic and arbitrary addresses, never the NIC's own, so
	// the NIC must accept packets not addressed to it (promiscuous) and endpoints
	// must be allowed to speak for those addresses (spoofing).
	if e := s.SetPromiscuousMode(egressNICID, true); e != nil {
		s.Close()
		return nil, fmt.Errorf("egress promiscuous: %s", e)
	}
	if e := s.SetSpoofing(egressNICID, true); e != nil {
		s.Close()
		return nil, fmt.Errorf("egress spoofing: %s", e)
	}
	gw := tcpip.AddrFrom4([4]byte(mustIP4(egressGatewayIP)))
	if e := s.AddProtocolAddress(egressNICID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: gw, PrefixLen: egressPrefixLen},
	}, stack.AddressProperties{}); e != nil {
		s.Close()
		return nil, fmt.Errorf("egress gateway address: %s", e)
	}
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: egressNICID})

	// Everything is in place; enabling attaches the link and starts the dispatchers.
	if e := s.EnableNIC(egressNICID); e != nil {
		s.Close()
		return nil, fmt.Errorf("egress enable nic: %s", e)
	}
	return s, nil
}
