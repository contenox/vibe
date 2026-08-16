//go:build linux

package libsandbox

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	llRead uint64 = unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_EXECUTE

	llWrite uint64 = unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER

	llReadWrite = llRead | llWrite

	llExec uint64 = unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE

	llDirOnly uint64 = unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER
)

var systemRuntimePaths = []string{
	"/usr",
	"/lib",
	"/lib64",
	"/bin",
	"/sbin",
	"/etc/ld.so.cache",
	"/etc/ld.so.conf",
	"/etc/ld.so.conf.d",
	"/etc/ld.so.preload",
	"/etc/nsswitch.conf",
	"/etc/resolv.conf",
	"/etc/hosts",
	"/etc/host.conf",
	"/etc/gai.conf",
	"/etc/services",
	"/etc/protocols",
	"/etc/passwd",
	"/etc/group",
	"/etc/localtime",
	"/etc/ssl",
	"/etc/ssl/certs",
	"/etc/pki",
	"/etc/ca-certificates",
	"/dev/zero",
	"/dev/random",
	"/dev/urandom",
	"/dev/tty",
}

var systemRuntimeWritableDevices = []string{
	"/dev/null",
	"/dev/full",
}

func supportedFS(abi int) uint64 {
	fs := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		fs |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		fs |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		fs |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return fs
}

func applyLandlock(plan isolationPlan) error {
	abi, err := landlockABI()
	if err != nil {
		return fmt.Errorf("query landlock abi: %w", err)
	}
	if abi < 1 {
		return ErrLandlockUnsupported
	}
	handled := supportedFS(abi)

	rulesetFD, err := createRuleset(handled)
	if err != nil {
		return err
	}
	defer unix.Close(rulesetFD)

	// Must exist: the one writable root.
	if err := addPathRule(rulesetFD, plan.Workspace, llReadWrite&handled, false); err != nil {
		return err
	}
	// Must exist: read+execute so it can be run and loaded.
	if err := addPathRule(rulesetFD, plan.Exec, llExec&handled, false); err != nil {
		return err
	}
	// Read-only system runtime; best-effort, absent entries are skipped.
	for _, p := range systemRuntimePaths {
		if err := addPathRule(rulesetFD, p, llRead&handled, true); err != nil {
			return err
		}
	}
	// Writable /dev nodes; skip-missing like the read-only runtime above.
	for _, p := range systemRuntimeWritableDevices {
		if err := addPathRule(rulesetFD, p, llReadWrite&handled, true); err != nil {
			return err
		}
	}
	// Necessity carve-outs at their declared mode; a missing path stays denied.
	for _, c := range plan.FS {
		access := llRead
		if c.Mode == ModeRW {
			access = llReadWrite
		}
		if err := addPathRule(rulesetFD, c.Path, access&handled, true); err != nil {
			return err
		}
	}

	// NO_NEW_PRIVS is a precondition of landlock_restrict_self, and seals the
	// wall against setuid re-privileging.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	return landlockRestrictSelf(rulesetFD)
}

// ErrLandlockUnsupported reports that the running kernel exposes no usable
// Landlock filesystem ABI; it fails closed rather than running unconfined.
var ErrLandlockUnsupported = errors.New("libsandbox: landlock unsupported by kernel")

func landlockABI() (int, error) {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	if e != 0 {
		if e == unix.ENOSYS {
			return 0, nil
		}
		return 0, e
	}
	return int(r), nil
}

func createRuleset(handled uint64) (int, error) {
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if e != 0 {
		return -1, fmt.Errorf("landlock_create_ruleset: %w", e)
	}
	return int(fd), nil
}

func addPathRule(rulesetFD int, path string, access uint64, skipMissing bool) error {
	if access == 0 {
		return nil
	}
	pathFD, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if skipMissing && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open %q for landlock rule: %w", path, err)
	}
	defer unix.Close(pathFD)

	// Directory-only rights are invalid (EINVAL) on a regular file, so mask them out here.
	var st unix.Stat_t
	if err := unix.Fstat(pathFD, &st); err != nil {
		return fmt.Errorf("stat %q for landlock rule: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &^= llDirOnly
		if access == 0 {
			return nil // nothing file-applicable left to grant
		}
	}

	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(pathFD)}
	_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(rulesetFD),
		uintptr(unix.LANDLOCK_RULE_PATH_BENEATH), uintptr(unsafe.Pointer(&attr)), 0, 0, 0)
	if e != 0 {
		return fmt.Errorf("landlock_add_rule %q: %w", path, e)
	}
	return nil
}

func landlockRestrictSelf(rulesetFD int) error {
	_, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), 0, 0)
	if e != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", e)
	}
	return nil
}
