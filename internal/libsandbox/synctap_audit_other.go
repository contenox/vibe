//go:build linux && !amd64 && !arm64

package libsandbox

// tapAuditArch is 0 on Linux arches this package does not carry an AUDIT_ARCH
// token for. buildTapFilter reads this as "no arch guard": it still taps by
// syscall number, accepting that on such an arch a compat-ABI syscall whose
// number collides with a tapped one could produce a spurious (but harmless —
// always CONTINUE) telemetry event. The two arches contenox targets (amd64,
// arm64) carry the precise token; this keeps every other Linux arch building.
const tapAuditArch = uint32(0)
