//go:build linux && arm64

package libsandbox

import "golang.org/x/sys/unix"

// tapAuditArch is the AUDIT_ARCH token the seccomp filter matches so it taps only
// native-ABI syscalls (see synctap_audit_amd64.go).
const tapAuditArch = uint32(unix.AUDIT_ARCH_AARCH64)
