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

var errEgressDenied = errors.New("libsandbox: egress denied")

const egressNICID = 1

type egressBridge struct {
	ctx          context.Context
	tracker      libtracker.ActivityTracker
	conn         *net.UnixConn
	policy       *egressPolicy
	dialer       *net.Dialer
	allowPrivate bool

	resolveMu sync.Mutex
	resolved  map[string]resolvedHost
}

type resolvedHost struct {
	ips []net.IP
	err error
}

func setupEgress(ctx context.Context, cmd *exec.Cmd, spec Spec, tracker libtracker.ActivityTracker) (int, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("%w: egress control socketpair: %v", ErrIsolation, err)
	}
	parentFD, childFD := pair[0], pair[1]

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

	// Signals the shim the stack is attached; a failed write means it is gone.
	if _, werr := b.conn.Write([]byte{'R'}); werr != nil {
		b.teardown(s, tunFD)
		return
	}

	<-b.ctx.Done()
	b.teardown(s, tunFD)
}

func (b *egressBridge) teardown(s *stack.Stack, tunFD int) {
	s.Close()
	s.Wait()
	unix.Close(tunFD)
	b.conn.Close()
}

func (b *egressBridge) recvTunFD() (int, error) {
	return recvOneFD(b.conn, "egress")
}

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
		// Allow-listed but unresolvable (synthetic range exhausted): a distinct failure, not a policy deny.
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

	if !b.policy.portAllowed(host, port) {
		reportErr(fmt.Errorf("%w: connect to %s:%d refused (port not in the carve-out's allowed ports)",
			errEgressDenied, host, port))
		r.Complete(true)
		end()
		return
	}

	// SSRF guard: resolve once (closing DNS rebinding) and refuse private ranges
	// unless AllowPrivateEgress opted in.
	vetted, rerr := b.resolveAndGuard(host)
	if rerr != nil {
		reportErr(rerr)
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

func pipeConns(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

func buildEgressStack(tunFD int, tcpH func(*tcp.ForwarderRequest), udpH udp.ForwarderHandler) (*stack.Stack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

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
	if e := s.CreateNICWithOptions(egressNICID, link, stack.NICOptions{Disabled: true}); e != nil {
		s.Close()
		return nil, fmt.Errorf("egress nic: %s", e)
	}
	// Promiscuous+spoofing: the agent targets synthetic addresses, never the
	// NIC's own.
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

	if e := s.EnableNIC(egressNICID); e != nil {
		s.Close()
		return nil, fmt.Errorf("egress enable nic: %s", e)
	}
	return s, nil
}
