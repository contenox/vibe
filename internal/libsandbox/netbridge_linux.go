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

	"github.com/contenox/contenox/libtracker"
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
// egress carve-out, or a connection to an address never resolved for one. Passed
// to reportErr so a deny is greppable telemetry, not a silent drop.
var errEgressDenied = errors.New("libsandbox: egress denied")

const egressNICID = 1

// egressBridge is the parent side of the network wall: a userspace TCP/IP stack
// (gVisor netstack) attached to the TUN fd the shim created in the agent's
// namespace. All agent traffic is bare IP over that fd — no host route is
// reachable except through this stack, which answers DNS only for carve-out
// hosts and dials only addresses it resolved for them.
//
// One bridge serves one agent for the life of ctx: created by setupEgress before
// the process starts, parks until the shim hands over the TUN fd, serves until
// ctx is done, then tears the stack down and closes the fd (reclaiming the
// agent's netns).
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

// resolvedHost caches the one-time resolution of a carve-out host. Caching
// closes DNS rebinding: a later connection reuses this result instead of
// re-resolving, so the address the SSRF guard vetted is the address dialed.
type resolvedHost struct {
	ips []net.IP
	err error
}

// setupEgress wires the egress bridge for a command whose spec declared network
// carve-outs: creates the parent-shim control socket, hands the child end to the
// command as an inherited fd, launches the bridge goroutine, and returns the fd
// number the shim will find the socket at. The bridge runs until ctx is done.
// Appends exactly one entry to cmd.ExtraFiles. Errors wrap ErrIsolation and
// leave the command unstarted — fail-closed, no half-wired egress.
func setupEgress(ctx context.Context, cmd *exec.Cmd, spec Spec, tracker libtracker.ActivityTracker) (int, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("%w: egress control socketpair: %v", ErrIsolation, err)
	}
	parentFD, childFD := pair[0], pair[1]

	// The child end goes to the command as an inherited fd; the bridge never
	// touches it, so its teardown can't race cmd.Start()'s read of the *os.File.
	childFile := os.NewFile(uintptr(childFD), "contenox-egress-shim")
	childFDNum := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, childFile)

	parentFile := os.NewFile(uintptr(parentFD), "contenox-egress-parent")
	conn, err := net.FileConn(parentFile)
	parentFile.Close() // FileConn dups the fd; drop our original
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
// socket on ctx cancellation so a parked receive unblocks even if the process
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
// dispatchers via gVisor's internal stop signal (not by closing the fd), so
// s.Wait returns before the TUN fd is closed — no window where a dispatcher
// reads a closed-or-reused descriptor.
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
// already close-on-exec. CLOEXEC is set atomically at receipt via
// MSG_CMSG_CLOEXEC (not a follow-up CloseOnExec call), closing the window where
// a concurrently exec'd sibling could inherit the fd and sniff/inject on it.
// Drops to unix.Recvmsg via conn.SyscallConn since ReadMsgUnix has no flags
// argument; ctx-cancellation still works via the runtime poller. label names the
// surface for error strings; the one data byte is ignored.
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
			return false // let the poller wait and retry
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
	return fds[0], nil
}

// handleDNS terminates the agent's DNS. Only :53 is served; all other UDP is
// left unhandled so the stack replies port-unreachable (no non-DNS UDP egress).
// Each query is decided against the allow-list and logged (reportChange/
// reportErr).
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
		// Allow-listed but unresolvable (synthetic range exhausted): a distinct
		// failure, not a policy deny.
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

// handleTCP terminates the agent's outbound TCP. The destination is the
// synthetic token the DNS step handed out; it maps back to the allow-listed
// host, that host is resolved once and SSRF-vetted, the real address is dialed
// from the parent's namespace, and bytes are piped. An unresolved destination, a
// disallowed port, or a host resolving inward (without AllowPrivateEgress) is
// refused with a RST. Every attempt is logged (reportChange/reportErr).
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

	// A carve-out that named ports is reachable only on those.
	if !b.policy.portAllowed(host, port) {
		reportErr(fmt.Errorf("%w: connect to %s:%d refused (port not in the carve-out's allowed ports)",
			errEgressDenied, host, port))
		r.Complete(true)
		end()
		return
	}

	// SSRF guard: resolve the carve-out once (cached, closing DNS rebinding) and
	// refuse a loopback/link-local/private/unspecified/multicast result unless
	// AllowPrivateEgress opted in — the wall against pivoting onto the host's own
	// network (169.254.169.254, localhost services, RFC1918 neighbours).
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

// resolveAndGuard resolves a carve-out host exactly once and applies the SSRF
// guard, caching the outcome for the bridge's life. Caching is load-bearing
// security: a hostname that rebinds to an internal address between DNS answer
// and TCP connect cannot slip a fresh, unvetted address past the guard.
// First-writer wins under a race.
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

// lookupAndVet resolves host in the host namespace and keeps only addresses
// safe to dial: with AllowPrivateEgress off, every non-public class (loopback,
// link-local incl. cloud metadata, private, unspecified, multicast) is dropped.
// A host resolving to nothing safe is refused as an egress deny.
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
// connection that comes up. Trying each preserves dual-stack robustness (e.g.
// both ::1 and 127.0.0.1) without ever re-resolving.
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

// pipeConns splices two connections until either direction ends, then closes
// both so the other copy unblocks: the whole data path once a connection is
// authorized (netstack endpoint on the agent side, real socket on the host side).
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
// link endpoint, a NIC in promiscuous+spoofing mode (accepts/answers for the
// agent's arbitrary destinations), a gateway address, a catch-all route, and
// TCP/UDP forwarders. Caller owns the TUN fd (fdbased does not close it).
//
// Ordering is load-bearing: fdbased starts delivering packets the moment the
// NIC is attached, so handlers are registered and the NIC configured while
// Disabled, then enabled last — every piece of state a delivered packet touches
// must be in place before the first packet can arrive.
func buildEgressStack(tunFD int, tcpH func(*tcp.ForwarderRequest), udpH udp.ForwarderHandler) (*stack.Stack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	// Register forwarders before any NIC exists, so the handler slot is set
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
