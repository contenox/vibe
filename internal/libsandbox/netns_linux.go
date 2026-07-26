//go:build linux

package libsandbox

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// bringLoopbackUp raises the loopback interface inside the current network
// namespace. A freshly cloned CLONE_NEWNET namespace contains only "lo", and it
// starts DOWN — so localhost is unusable until it is brought up. Many toolchains
// bind 127.0.0.1 to talk to their own helper subprocesses (a language server, a
// test runner's coordinator), so the net wall raises "lo" while leaving the
// namespace otherwise routeless: host-local traffic works, nothing leaves the
// box. The rest of the network stays absent by construction.
//
// It is CGo-free: an AF_INET datagram socket (creatable in an empty netns — it
// needs no interface) plus the classic SIOCGIFFLAGS/SIOCSIFFLAGS ifreq ioctl,
// which is exactly what "ip link set lo up" does underneath. The child holds
// CAP_NET_ADMIN over this netns (it created it inside the user namespace it also
// created), so SIOCSIFFLAGS is permitted unprivileged. Failure is returned so
// the shim can fail closed rather than run the agent in a half-set-up namespace.
//
// It does not require the locked OS thread the Landlock/execve sequence does — a
// netns is a process-wide property, so the socket and ioctls are valid on any
// thread — and the shim calls it before pinning the thread.
func bringLoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open netlink/ioctl control socket: %w", err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("build ifreq for lo: %w", err)
	}
	// Read the current flags, then OR in IFF_UP and write them back — a
	// read-modify-write so we do not clobber flags the kernel already set on the
	// loopback device (e.g. IFF_LOOPBACK).
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("get lo flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("set lo up: %w", err)
	}
	return nil
}
