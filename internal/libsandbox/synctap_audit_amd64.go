//go:build linux && amd64

package libsandbox

import "golang.org/x/sys/unix"

// tapAuditArch is the AUDIT_ARCH token the seccomp filter matches so it taps only
// native-ABI syscalls (a compat/foreign ABI numbers syscalls differently, so its
// execve nr would be a different call under our arch). Non-zero enables the arch
// guard in buildTapFilter.
const tapAuditArch = uint32(unix.AUDIT_ARCH_X86_64)
