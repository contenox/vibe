//go:build linux

package libsandbox

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Access-right groups over the Landlock filesystem ABI. They are expressed with
// every right the mechanism knows about and masked to the running kernel's
// supported subset at apply time (supportedFS) — so the same code degrades
// cleanly from an ABI-1 kernel to a current one without EINVAL from an
// unsupported bit. The constants are typed uint64 so they compose with the
// syscall attrs directly.
const (
	// llRead is read-only reach: open files for reading, list directories, and
	// execute — the grant for a ModeRO carve-out and for the read-only system
	// runtime (loader + libraries) a dynamically linked agent needs to start.
	llRead uint64 = unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_EXECUTE

	// llWrite is the mutating rights layered on top of llRead for a writable hole:
	// write, truncate, create the node types real tooling needs (regular files,
	// dirs, sockets, fifos, symlinks), rename/link (REFER), and remove.
	//
	// It deliberately does NOT grant MAKE_CHAR, MAKE_BLOCK, or IOCTL_DEV. The agent
	// holds CAP_MKNOD inside its userns, so granting MAKE_CHAR/MAKE_BLOCK on the
	// workspace would let it create device nodes (a mknod'd /dev/sda or a raw char
	// device is a classic confinement-escape primitive); no legitimate agent
	// tooling mknods a device in a workspace. IOCTL_DEV is dropped for the same
	// least-privilege reason — the workspace holds no device to ioctl. These three
	// rights stay in the ruleset's HANDLED set (supportedFS) but are granted
	// NOWHERE, so the kernel denies them everywhere; dropping them from `handled`
	// instead would leave the operations ungoverned (allowed), which is the exact
	// opposite of the intent.
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

	// llExec is the minimal grant that lets the confined target binary be
	// execve'd and read by the dynamic loader. It is an implicit necessity, not a
	// spec carve-out: an agent whose own binary is unreadable cannot run at all.
	llExec uint64 = unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE

	// llDirOnly is the set of Landlock rights that only apply to a DIRECTORY.
	// landlock_add_rule returns EINVAL if any of these is requested on a
	// path_beneath rule whose target is a regular file, so addPathRule masks them
	// out when the target is not a directory — which is what lets the system
	// runtime set and carve-outs name individual FILES (e.g. /etc/ld.so.cache)
	// alongside directories. On a directory none of these is stripped, so the
	// workspace's create/remove rights are unaffected.
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

// systemRuntimePaths is the read-only system surface a real (dynamically linked)
// agent must reach merely to start: the ELF interpreter and shared libraries, the
// stock executables its Bash will run, a MINIMAL set of /etc files the loader,
// name resolution, and common tooling actually read, and the core read-only /dev
// character devices (/dev/zero, /dev/random, /dev/urandom, /dev/tty). It is NOT a spec carve-out
// and holds none of the loot paths the wall exists to deny — those all live under
// the operator's real $HOME (~/.ssh, ~/.aws, ~/.npmrc, ~/.contenox), which is not
// on this list and not under the scoped HOME. Granted read+execute, read-only.
// Missing entries (merged-/usr layouts, absent /sbin, an /etc file this distro
// lacks) are skipped — a rule cannot name an absent inode, and an absent path
// stays denied, which is the correct outcome. Empirically required: without the
// libs/loader entries a dynamically linked binary fails at execve/loader time
// under Landlock.
//
// The /etc grant is deliberately NOT the whole directory. Whole-/etc read is a
// broad grant (shadow-adjacent files, machine identifiers, every service's config)
// that the agent does not need to boot, so the security-forward default is to name
// only the files the loader and stock tooling read:
//   - dynamic loader: ld.so.cache/conf/conf.d/preload
//   - name resolution: nsswitch.conf, resolv.conf, hosts, host.conf, gai.conf
//   - protocol/service tables: services, protocols
//   - identity mapping (id/ls/git author): passwd, group
//   - timezone: localtime
//   - TLS trust store (git/npm/curl over HTTPS): ssl, ssl/certs, pki, ca-certificates
//
// It is a floor, not a ceiling: an agent whose toolchain reads some other /etc
// file gets that file added per-agent (a carve-out or an extension of this set),
// never a reversion to the whole directory.
var systemRuntimePaths = []string{
	"/usr",
	"/lib",
	"/lib64",
	"/bin",
	"/sbin",
	// Minimal /etc surface (skip-missing) — NOT the whole directory.
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
	// Minimal /dev character-device floor (skip-missing), read-only half — the
	// stateless nodes every POSIX toolchain assumes exist merely to run, NOT loot
	// paths. The writable half (/dev/null, /dev/full) is granted read-write
	// separately (systemRuntimeWritableDevices), because this slice is granted
	// llRead. An absent node is skipped, like any other system-runtime path.
	"/dev/zero",
	"/dev/random",
	"/dev/urandom",
	"/dev/tty",
}

// systemRuntimeWritableDevices is the WRITABLE half of the /dev floor: character
// devices tooling opens for WRITING, not just reading. /dev/null is load-bearing —
// a confined Bash sources init snapshots and runs "cmd 2>/dev/null" on nearly every
// command, and countless tools open it at startup; /dev/full is the standard
// ENOSPC-behaviour probe. They are granted llReadWrite, but on a char-device node
// addPathRule strips the directory-only rights and llWrite never carried
// MAKE_CHAR/MAKE_BLOCK/IOCTL_DEV (see TestUnit_llWrite_NoDeviceNodeCreation), so the
// net grant is READ_FILE|WRITE_FILE|TRUNCATE — opening an EXISTING node, never
// minting a new device. Skip-missing, like systemRuntimePaths.
var systemRuntimeWritableDevices = []string{
	"/dev/null",
	"/dev/full",
}

// supportedFS returns the filesystem access rights the given Landlock ABI
// supports. ABI 1 (kernel 5.13) has the base set; REFER arrived in 2, TRUNCATE
// in 3, IOCTL_DEV in 5. Masking the handled/allowed rights to this keeps
// landlock_create_ruleset from rejecting a bit the kernel does not know.
//
// This is the ruleset's HANDLED set — every right listed here is DENIED unless a
// rule re-grants it. It deliberately still handles MAKE_CHAR, MAKE_BLOCK, and
// IOCTL_DEV even though llWrite no longer grants them (FIX 5): keeping them handled
// is exactly what makes device-node creation and device ioctls denied everywhere.
// Removing them here would UN-govern those operations, not restrict them.
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
// the workspace read-write, the target binary read+execute, each carve-out at
// its mode, and the read-only system runtime — nothing else. It is fail-closed:
// a kernel without a usable Landlock fs ABI yields ErrIsolation (wrapping
// ErrLandlockUnsupported) rather than a silently unconfined process. It must run
// on a locked OS thread (see ShimMain), because Landlock and the execve that
// follows are per-thread and must be the same thread.
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
	// The read-only system runtime (loader, libs, stock tools, ro /dev nodes).
	// Best-effort: entries absent on this layout are simply skipped.
	for _, p := range systemRuntimePaths {
		if err := addPathRule(rulesetFD, p, llRead&handled, true); err != nil {
			return err
		}
	}
	// The writable /dev character devices (/dev/null, /dev/full): granted
	// read-write because tooling writes to them. On a char-device file the
	// llReadWrite grant reduces to READ_FILE|WRITE_FILE|TRUNCATE (addPathRule masks
	// the dir-only rights; llWrite never held MAKE_CHAR/MAKE_BLOCK/IOCTL_DEV), so an
	// existing node is opened for writing without conferring device-node creation.
	// Skip-missing, like the read-only runtime above.
	for _, p := range systemRuntimeWritableDevices {
		if err := addPathRule(rulesetFD, p, llReadWrite&handled, true); err != nil {
			return err
		}
	}
	// The necessity carve-outs, each at its declared mode. A carve-out path that
	// does not exist cannot be referenced by a rule and stays denied — skip it.
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

// addPathRule grants access beneath path in the ruleset. The path is opened
// O_PATH (no read permission needed, symlinks followed to the real inode, which
// is what execve/open will be checked against). When skipMissing is set, a
// non-existent path is not an error: a rule cannot name an absent inode, and an
// absent path stays denied, which is the correct outcome for an optional
// carve-out or a system path this layout lacks.
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

	// Directory-only rights are invalid on a regular file (landlock_add_rule
	// returns EINVAL), so mask them out when the target is not a directory. This is
	// what lets systemRuntimePaths and carve-outs name individual files as well as
	// directories; on a directory nothing is stripped.
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
