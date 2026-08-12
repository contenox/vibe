//go:build linux && amd64

package libsandbox

import "golang.org/x/sys/unix"

const tapAuditArch = uint32(unix.AUDIT_ARCH_X86_64)
