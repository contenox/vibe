//go:build linux && amd64

package libsandbox

import "golang.org/x/sys/unix"

// tapAuditArch is the AUDIT_ARCH token the seccomp filter matches to tap only
// native-ABI syscalls. Non-zero enables the arch guard in buildTapFilter.
const tapAuditArch = uint32(unix.AUDIT_ARCH_X86_64)
