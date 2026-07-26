//go:build linux

package libsandbox

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestUnit_llWrite_NoDeviceNodeCreation is the FIX 5 regression: the writable
// grant must NOT confer device-node creation (MAKE_CHAR/MAKE_BLOCK) or device
// ioctls (IOCTL_DEV) — the agent holds CAP_MKNOD in its userns, so granting those
// would let it mint device nodes as an escape primitive. It must still confer the
// node types real tooling needs.
func TestUnit_llWrite_NoDeviceNodeCreation(t *testing.T) {
	forbidden := map[string]uint64{
		"MAKE_CHAR":  unix.LANDLOCK_ACCESS_FS_MAKE_CHAR,
		"MAKE_BLOCK": unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK,
		"IOCTL_DEV":  unix.LANDLOCK_ACCESS_FS_IOCTL_DEV,
	}
	for name, bit := range forbidden {
		if llWrite&bit != 0 {
			t.Errorf("llWrite still grants %s (%#x): device-node/ioctl rights must not be granted", name, bit)
		}
	}

	required := map[string]uint64{
		"WRITE_FILE":  unix.LANDLOCK_ACCESS_FS_WRITE_FILE,
		"TRUNCATE":    unix.LANDLOCK_ACCESS_FS_TRUNCATE,
		"REMOVE_DIR":  unix.LANDLOCK_ACCESS_FS_REMOVE_DIR,
		"REMOVE_FILE": unix.LANDLOCK_ACCESS_FS_REMOVE_FILE,
		"MAKE_DIR":    unix.LANDLOCK_ACCESS_FS_MAKE_DIR,
		"MAKE_REG":    unix.LANDLOCK_ACCESS_FS_MAKE_REG,
		"MAKE_SOCK":   unix.LANDLOCK_ACCESS_FS_MAKE_SOCK,
		"MAKE_FIFO":   unix.LANDLOCK_ACCESS_FS_MAKE_FIFO,
		"MAKE_SYM":    unix.LANDLOCK_ACCESS_FS_MAKE_SYM,
		"REFER":       unix.LANDLOCK_ACCESS_FS_REFER,
	}
	for name, bit := range required {
		if llWrite&bit == 0 {
			t.Errorf("llWrite no longer grants %s (%#x): real tooling needs it", name, bit)
		}
	}
}

// TestUnit_supportedFS_StillHandlesDeviceRights proves the other half of FIX 5:
// the device-node rights stay in the HANDLED set, so the kernel actively DENIES
// them (a handled-but-never-granted right is denied; an unhandled right is
// allowed). If these were dropped from `handled`, device creation would silently
// become permitted again.
func TestUnit_supportedFS_StillHandlesDeviceRights(t *testing.T) {
	handled := supportedFS(5) // ABI 5+ is where IOCTL_DEV exists
	for name, bit := range map[string]uint64{
		"MAKE_CHAR":  unix.LANDLOCK_ACCESS_FS_MAKE_CHAR,
		"MAKE_BLOCK": unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK,
		"IOCTL_DEV":  unix.LANDLOCK_ACCESS_FS_IOCTL_DEV,
	} {
		if handled&bit == 0 {
			t.Errorf("supportedFS no longer HANDLES %s (%#x): the operation would be ungoverned (allowed), not denied", name, bit)
		}
	}
}

// TestUnit_DeviceFloor_Composition pins the /dev floor's split so a careless edit
// cannot quietly break it: /dev/null must be in the writable half (a read-only
// /dev/null breaks "cmd 2>/dev/null" for a confined Bash), and the two halves must
// stay disjoint so each device's intended access is declared in exactly one place.
func TestUnit_DeviceFloor_Composition(t *testing.T) {
	if len(systemRuntimeWritableDevices) == 0 {
		t.Fatal("systemRuntimeWritableDevices is empty: /dev/null must be granted writable")
	}

	ro := make(map[string]bool, len(systemRuntimePaths))
	for _, p := range systemRuntimePaths {
		ro[p] = true
	}

	haveNull := false
	for _, d := range systemRuntimeWritableDevices {
		if ro[d] {
			t.Errorf("%s appears in BOTH systemRuntimePaths and systemRuntimeWritableDevices: declare each device's access in one place", d)
		}
		if d == "/dev/null" {
			haveNull = true
		}
	}
	if !haveNull {
		t.Error("/dev/null must be in systemRuntimeWritableDevices (the load-bearing writable device)")
	}
}
