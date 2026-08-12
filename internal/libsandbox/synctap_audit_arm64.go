//go:build linux && arm64

package libsandbox

import "golang.org/x/sys/unix"

const tapAuditArch = uint32(unix.AUDIT_ARCH_AARCH64)
