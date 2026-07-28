//go:build linux

package libsandbox

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Access-right groups over the Landlock filesystem ABI, expressed with every
// right the mechanism knows and masked to the running kernel's supported
// subset at apply time (supportedFS) so it degrades cleanly on older ABIs.
const (
	// llRead: open for reading, list directories, execute. Grant for a ModeRO
	// carve-out and the read-only system runtime.
	llRead uint64 = unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_EXECUTE

	// llWrite layers mutating rights on llRead: write, truncate, create
	// regular/dir/sock/fifo/symlink nodes, rename/link (REFER), remove.
	// Deliberately excludes MAKE_CHAR, MAKE_BLOCK, IOCTL_DEV: the agent holds
	// CAP_MKNOD in its userns, so granting device-node creation would be a
	// confinement-escape primitive. These stay HANDLED (supportedFS) but
	// granted nowhere, so the kernel denies them everywhere rather than
	// leaving them ungoverned.
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

	// llReadWrite is the grant for the workspace and a ModeRW carve-out.
	llReadWrite = llRead | llWrite

	// llExec lets the target binary be execve'd and read by the dynamic
	// loader — an implicit necessity, not a spec carve-out.
	llExec uint64 = unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE

	// llDirOnly is the set of rights valid only on a directory: landlock_add_rule
	// returns EINVAL if any is requested on a regular-file path_beneath target,
	// so addPathRule masks them out for non-directories.
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

// systemRuntimePaths is the read-only system surface a dynamically linked agent
// needs merely to start: ELF interpreter/libs, stock executables, a minimal
// /etc set (loader, name resolution, TLS trust store — deliberately not the
// whole directory, to avoid shadow-adjacent files and machine identifiers),
// and core read-only /dev nodes. Not a spec carve-out; holds none of the loot
// paths under the operator's real $HOME. Granted read+execute. Missing entries
// are skipped (an absent path stays denied, the correct outcome). This is a
// floor: an agent needing another /etc file gets it added, never a reversion
// to whole-directory access.
var systemRuntimePaths = []string{
	"/usr",
	"/lib",
	"/lib64",
	"/bin",
	"/sbin",
	// Minimal /etc surface (skip-missing), not the whole directory.
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
	// Minimal /dev character-device floor (skip-missing), read-only half; the
	// writable half is granted separately (systemRuntimeWritableDevices).
	"/dev/zero",
	"/dev/random",
	"/dev/urandom",
	"/dev/tty",
}

// systemRuntimeWritableDevices is the writable half of the /dev floor
// (/dev/null, /dev/full — load-bearing for shell redirection and ENOSPC
// probes). Granted llReadWrite, but on a char-device node addPathRule strips
// directory-only rights and llWrite never carried MAKE_CHAR/MAKE_BLOCK/
// IOCTL_DEV, so the net grant only opens an existing node, never mints a new
// device. Skip-missing, like systemRuntimePaths.
var systemRuntimeWritableDevices = []string{
	"/dev/null",
	"/dev/full",
}

// supportedFS returns the filesystem access rights the given Landlock ABI
// supports: ABI 1 (kernel 5.13) has the base set, REFER arrived in 2, TRUNCATE
// in 3, IOCTL_DEV in 5. Masking to this keeps landlock_create_ruleset from
// rejecting a bit the kernel doesn't know. This is the ruleset's HANDLED set —
// every right here is denied unless a rule re-grants it; MAKE_CHAR/MAKE_BLOCK/
// IOCTL_DEV stay handled (though never granted) so those ops stay governed.
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

// applyLandlock builds and installs the filesystem wall for the current thread:
// workspace read-write, target binary read+execute, each carve-out at its mode,
// and the read-only system runtime — nothing else. Fail-closed: a kernel
// without a usable Landlock fs ABI yields ErrIsolation (wrapping
// ErrLandlockUnsupported) rather than an unconfined process. Must run on a
// locked OS thread (see ShimMain): Landlock and the following execve are
// per-thread and must be the same thread.
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

	// The workspace: the one writable root. Must exist.
	if err := addPathRule(rulesetFD, plan.Workspace, llReadWrite&handled, false); err != nil {
		return err
	}
	// The target binary: read+execute so it can be run and loaded. Must exist.
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

	// NO_NEW_PRIVS is a precondition of landlock_restrict_self for an unprivileged
	// process, and it also seals the wall against re-privileging via setuid execs.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	return landlockRestrictSelf(rulesetFD)
}

// ErrLandlockUnsupported reports that the running kernel exposes no usable
// Landlock filesystem ABI. It is wrapped in ErrIsolation by the shim; the wall
// is fail-closed, so the agent is refused rather than run unconfined.
var ErrLandlockUnsupported = errors.New("libsandbox: landlock unsupported by kernel")

// landlockABI returns the kernel's Landlock ABI version (>=1 when supported), or
// 0 when Landlock is absent (ENOSYS). Querying it is landlock_create_ruleset
// with the VERSION flag and a nil attr.
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

// createRuleset creates an empty ruleset that handles the given fs access rights
// (everything is denied unless a rule re-grants it) and returns its fd.
func createRuleset(handled uint64) (int, error) {
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if e != 0 {
		return -1, fmt.Errorf("landlock_create_ruleset: %w", e)
	}
	return int(fd), nil
}

// addPathRule grants access beneath path in the ruleset. Opened O_PATH
// (symlinks followed to the real inode checked at execve/open). When
// skipMissing is set, a non-existent path is not an error — it just stays
// denied, the correct outcome for an optional carve-out or absent system path.
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

	// Directory-only rights are invalid (EINVAL) on a regular file, so mask
	// them out for non-directories.
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

// landlockRestrictSelf enforces the ruleset on the calling thread. After this
// returns, every fs access not granted by a rule is denied (EACCES/EPERM) for
// this thread and — across the imminent execve — for the confined program.
func landlockRestrictSelf(rulesetFD int) error {
	_, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), 0, 0)
	if e != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", e)
	}
	return nil
}
