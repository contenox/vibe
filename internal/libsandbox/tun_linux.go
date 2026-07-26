//go:build linux

package libsandbox

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Egress network addressing. These addresses live only inside the agent's fresh
// network namespace (they are never plumbed on the host), so any RFC1918 range
// is safe; a 10.x range unlikely to collide with a developer LAN is used so that,
// were the numbers ever to leak into a log, they read as clearly sandbox-local.
//
//   - egressTunName   the L3 device the agent routes through.
//   - egressAgentIP   the agent's own address on that device.
//   - egressGatewayIP the parent netstack's address; the agent's DNS resolver and
//     the nominal next hop. The netstack answers for this and (in promiscuous
//     mode) for every other destination the agent tries to reach.
//   - egressSynthNet  the /16 the DNS server hands out synthetic addresses from,
//     one per allow-listed hostname (see netbridge_linux.go). Disjoint from the
//     device subnet so a synthetic address never aliases the gateway or agent.
const (
	egressTunName   = "ctxsbx0"
	egressAgentIP   = "10.191.0.2"
	egressGatewayIP = "10.191.0.1"
	egressPrefixLen = 24
	egressMTU       = 1500
	egressDNSPort   = 53

	// egressResolverEnv names the resolver address handed to the confined agent
	// (host:port). It is advisory — DNS enforcement does not depend on the agent
	// honoring it, because the netstack intercepts every :53 datagram regardless
	// of destination — but pointing a well-behaved resolver here keeps its queries
	// on the fast path and its failures legible.
	egressResolverEnv = "CONTENOX_SANDBOX_DNS"
)

// createEgressTun creates the agent's egress device inside the current (shim)
// network namespace and returns an open fd referencing it. The shim runs as
// root-in-userns and therefore holds CAP_NET_ADMIN over the namespace it created
// (the same capability that raises "lo"), so every step here is permitted for an
// otherwise unprivileged process — no host root, no CGo.
//
// It is an L3 TUN (IFF_TUN), not an L2 TAP: the agent's traffic is carried as
// bare IP packets with no Ethernet framing, so the parent's userspace stack needs
// no ARP and the namespace needs no synthetic MAC/neighbor state. The device is
// given egressAgentIP/egressPrefixLen, brought up, and made the namespace's
// default route (a scope-link route out the device — an L3 TUN needs no gateway
// address to forward, every packet is simply written to the fd). The returned fd
// is what the parent attaches its netstack to; passing it out (SCM_RIGHTS) and
// closing every in-shim copy is what keeps the device alive after the shim
// execve's the agent, while making it unreachable to the agent itself.
func createEgressTun() (int, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	// TUNSETIFF binds the fd to a new L3 device. IFF_NO_PI drops the 4-byte
	// packet-info prefix so what we read/write is exactly an IP datagram.
	ifr, err := unix.NewIfreq(egressTunName)
	if err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("ifreq %q: %w", egressTunName, err)
	}
	ifr.SetUint16(uint16(unix.IFF_TUN | unix.IFF_NO_PI))
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("TUNSETIFF %q: %w", egressTunName, err)
	}

	if err := configureEgressTun(); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// establishEgress is the shim's egress step, run inside the fresh netns after
// "lo" is up and before Landlock (which would deny /dev/net/tun) and before the
// thread is pinned (netns setup is process-wide). It creates the TUN, hands its
// fd to the parent over the inherited control socket, and blocks until the parent
// signals its userspace stack is attached — so the agent never runs before its
// only route is being served. It then closes every in-shim copy of both fds: the
// TUN stays alive on the parent's received copy while becoming unreachable to the
// agent, and the control socket does not leak across the execve.
//
// Fail-closed: any error (TUN creation, fd hand-off, a parent that closed without
// acknowledging) is returned so ShimMain refuses to exec the agent rather than
// run it with an unserved or absent route.
func establishEgress(sockFD int) error {
	tunFD, err := createEgressTun()
	if err != nil {
		return err
	}
	if err := sendTunFD(sockFD, tunFD); err != nil {
		unix.Close(tunFD)
		return err
	}
	unix.Close(tunFD)  // the parent holds it now; keep it out of the agent's reach
	unix.Close(sockFD) // do not leak the control socket into the exec'd agent
	return nil
}

// sendTunFD passes tunFD to the parent as an SCM_RIGHTS ancillary message over the
// inherited unix socket, then blocks on a one-byte readiness ack. The ack is the
// handshake that orders bring-up: the parent has received the fd and attached its
// stack before the shim proceeds to exec the agent, eliminating a race where the
// agent's first packet could arrive before anything is listening. A closed socket
// (parent gave up, e.g. its stack failed to build) surfaces as EOF and fails the
// shim closed.
func sendTunFD(sockFD, tunFD int) error {
	if err := unix.Sendmsg(sockFD, []byte{'T'}, unix.UnixRights(tunFD), nil, 0); err != nil {
		return fmt.Errorf("hand TUN fd to parent: %w", err)
	}
	ack := make([]byte, 1)
	n, err := unix.Read(sockFD, ack)
	if err != nil {
		return fmt.Errorf("await egress readiness: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("egress parent closed before readiness")
	}
	return nil
}

// configureEgressTun assigns the device its address/netmask, raises it, and
// installs the default route — the netns-side plumbing that makes the agent's
// stack send everything out the TUN. It opens a throwaway AF_INET socket for the
// address ioctls (valid in an otherwise routeless netns) and a netlink socket for
// the route. All of it needs CAP_NET_ADMIN, which the shim holds over its netns.
func configureEgressTun() error {
	ctl, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("egress control socket: %w", err)
	}
	defer unix.Close(ctl)

	addr, err := unix.NewIfreq(egressTunName)
	if err != nil {
		return fmt.Errorf("ifreq addr: %w", err)
	}
	if err := addr.SetInet4Addr(mustIP4(egressAgentIP)); err != nil {
		return fmt.Errorf("set addr bytes: %w", err)
	}
	if err := unix.IoctlIfreq(ctl, unix.SIOCSIFADDR, addr); err != nil {
		return fmt.Errorf("SIOCSIFADDR %s: %w", egressAgentIP, err)
	}

	mask, err := unix.NewIfreq(egressTunName)
	if err != nil {
		return fmt.Errorf("ifreq mask: %w", err)
	}
	if err := mask.SetInet4Addr(prefixMask(egressPrefixLen)); err != nil {
		return fmt.Errorf("set mask bytes: %w", err)
	}
	if err := unix.IoctlIfreq(ctl, unix.SIOCSIFNETMASK, mask); err != nil {
		return fmt.Errorf("SIOCSIFNETMASK /%d: %w", egressPrefixLen, err)
	}

	flags, err := unix.NewIfreq(egressTunName)
	if err != nil {
		return fmt.Errorf("ifreq flags: %w", err)
	}
	if err := unix.IoctlIfreq(ctl, unix.SIOCGIFFLAGS, flags); err != nil {
		return fmt.Errorf("SIOCGIFFLAGS: %w", err)
	}
	flags.SetUint16(flags.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(ctl, unix.SIOCSIFFLAGS, flags); err != nil {
		return fmt.Errorf("SIOCSIFFLAGS up: %w", err)
	}

	idxReq, err := unix.NewIfreq(egressTunName)
	if err != nil {
		return fmt.Errorf("ifreq index: %w", err)
	}
	if err := unix.IoctlIfreq(ctl, unix.SIOCGIFINDEX, idxReq); err != nil {
		return fmt.Errorf("SIOCGIFINDEX: %w", err)
	}
	return addDefaultRouteDev(int32(idxReq.Uint32()))
}

// addDefaultRouteDev installs "default dev <ifindex>" (scope link, no gateway) via
// netlink RTM_NEWROUTE. A device-scoped default route is exactly what an L3 TUN
// wants: there is no next-hop address to resolve, so every packet the agent sends
// to any address is matched by this route and written to the TUN fd, where the
// parent's netstack picks it up. Netlink (rather than the fixed-layout SIOCADDRT
// rtentry, whose struct is awkward and arch-sensitive) keeps this CGo-free and
// portable across 32/64-bit.
func addDefaultRouteDev(ifindex int32) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}

	rtmsg := unix.RtMsg{
		Family:   unix.AF_INET,
		Dst_len:  0, // 0.0.0.0/0 — the default route
		Table:    unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_BOOT,
		Scope:    unix.RT_SCOPE_LINK,
		Type:     unix.RTN_UNICAST,
	}
	// One attribute: RTA_OIF (output interface index).
	oif := make([]byte, unix.SizeofRtAttr+4)
	binary.NativeEndian.PutUint16(oif[0:], uint16(unix.SizeofRtAttr+4))
	binary.NativeEndian.PutUint16(oif[2:], unix.RTA_OIF)
	binary.NativeEndian.PutUint32(oif[4:], uint32(ifindex))

	payload := append(rtMsgBytes(rtmsg), oif...)

	hdr := unix.NlMsghdr{
		Len:   uint32(unix.SizeofNlMsghdr + len(payload)),
		Type:  unix.RTM_NEWROUTE,
		Flags: unix.NLM_F_REQUEST | unix.NLM_F_CREATE | unix.NLM_F_ACK,
		Seq:   1,
	}
	msg := append(nlMsghdrBytes(hdr), payload...)

	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink send route: %w", err)
	}
	return readNetlinkAck(fd)
}

// readNetlinkAck reads the kernel's reply to an NLM_F_ACK request and turns a
// non-zero errno into a Go error, so a rejected route fails the shim closed
// rather than silently leaving the agent unroutable. The reply to a single acked
// request is one NLMSG_ERROR message: an NlMsghdr followed by an int32 errno
// (0 on success) and the echoed request header. The layout is fixed and small,
// so it is decoded directly rather than pulling in syscall.ParseNetlinkMessage.
func readNetlinkAck(fd int) error {
	buf := make([]byte, 4096)
	n, err := unix.Read(fd, buf)
	if err != nil {
		return fmt.Errorf("netlink ack read: %w", err)
	}
	if n < unix.SizeofNlMsghdr+4 {
		return fmt.Errorf("netlink ack short: %d bytes", n)
	}
	msgType := binary.NativeEndian.Uint16(buf[4:6])
	if msgType != unix.NLMSG_ERROR {
		// Not an error message (e.g. NLMSG_DONE) — nothing to fail on.
		return nil
	}
	errno := int32(binary.NativeEndian.Uint32(buf[unix.SizeofNlMsghdr:]))
	if errno != 0 {
		return fmt.Errorf("netlink route rejected: %w", unix.Errno(-errno))
	}
	return nil
}

func rtMsgBytes(m unix.RtMsg) []byte {
	b := make([]byte, unix.SizeofRtMsg)
	*(*unix.RtMsg)(unsafe.Pointer(&b[0])) = m
	return b
}

func nlMsghdrBytes(h unix.NlMsghdr) []byte {
	b := make([]byte, unix.SizeofNlMsghdr)
	*(*unix.NlMsghdr)(unsafe.Pointer(&b[0])) = h
	return b
}

// prefixMask returns the 4-byte network mask for an IPv4 prefix length.
func prefixMask(prefix int) []byte {
	var m uint32
	if prefix > 0 {
		m = ^uint32(0) << (32 - prefix)
	}
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, m)
	return out
}
