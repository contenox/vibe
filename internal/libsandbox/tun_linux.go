//go:build linux

package libsandbox

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	egressTunName   = "ctxsbx0"
	egressAgentIP   = "10.191.0.2"
	egressGatewayIP = "10.191.0.1"
	egressPrefixLen = 24
	egressMTU       = 1500
	egressDNSPort   = 53

	egressResolverEnv = "CONTENOX_SANDBOX_DNS"
)

func createEgressTun() (int, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	// TUNSETIFF binds the fd to a new L3 device; IFF_NO_PI drops the 4-byte packet-info prefix so reads/writes are exactly an IP datagram.
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

func prefixMask(prefix int) []byte {
	var m uint32
	if prefix > 0 {
		m = ^uint32(0) << (32 - prefix)
	}
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, m)
	return out
}
