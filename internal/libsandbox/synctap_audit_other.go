//go:build linux && !amd64 && !arm64

package libsandbox

// tapAuditArch is 0 on arches without a carried AUDIT_ARCH token.
// buildTapFilter reads this as "no arch guard": it still taps by syscall
// number, accepting a possible spurious (but harmless, always-CONTINUE) event
// on a colliding compat-ABI syscall.
const tapAuditArch = uint32(0)
