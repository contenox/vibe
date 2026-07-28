//go:build linux

package libsandbox

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// bringLoopbackUp raises the loopback interface inside the current network
// namespace. A freshly cloned CLONE_NEWNET namespace contains only "lo",
// starting DOWN; many toolchains bind 127.0.0.1 to talk to their own helper
// subprocesses, so the wall raises "lo" while leaving the namespace otherwise
// routeless. CGo-free (AF_INET socket + SIOCGIFFLAGS/SIOCSIFFLAGS ioctl,
// permitted unprivileged since the child holds CAP_NET_ADMIN over its netns).
// Failure is returned so the shim fails closed. Does not need the locked OS
// thread the Landlock/execve sequence does (netns is process-wide), so the
// shim calls it before pinning the thread.
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
	// Read-modify-write so existing kernel-set flags (e.g. IFF_LOOPBACK) survive.
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("get lo flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("set lo up: %w", err)
	}
	return nil
}
