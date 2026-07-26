//go:build linux

package libsandbox

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"unsafe"

	"github.com/contenox/beam/internal/libtracker"
	"golang.org/x/sys/unix"
)

// This file implements SLICE 5: a seccomp user-notify TELEMETRY TAP. The
// deny-by-construction floor (Landlock + netns) is telemetry-poor — a denied
// exec is a silent EPERM the tracker never sees (blueprint §3, §5). The tap turns
// a small enumerated set of syscalls into RECORDED libtracker events so an
// operator can watch whether an agent tries to bypass the wall, WITHOUT the tap
// itself deciding anything: every notification is recorded and then answered with
// SECCOMP_USER_NOTIF_FLAG_CONTINUE, so the kernel runs the real syscall and
// Landlock/netns still deny whatever they deny. Recording-then-CONTINUE is exactly
// the use the blueprint blesses as TOCTOU-safe: the hazard "inspect args, then
// CONTINUE on a security decision" never arises because the tap makes no decision.
//
// The mechanism has two halves that talk over an inherited unix socket, reusing
// the SCM_RIGHTS + readiness-ack pattern of the egress bridge (netbridge_linux.go /
// tun_linux.go):
//
//   - shim side (installSyscallTap): build the BPF program, set NO_NEW_PRIVS,
//     install the filter with SECCOMP_FILTER_FLAG_NEW_LISTENER to obtain the notify
//     fd, SCM_RIGHTS it to the parent, and block on the parent's ack before the
//     shim's own (tapped) execve — so the supervisor is reading before the agent
//     can make a tapped syscall.
//   - parent side (tapSupervisor): receive the notify fd, ack, then loop
//     RECV → record a libtracker event → SEND CONTINUE, bound to ctx.

// tapSubject is the libtracker subject for every syscall-tap event — the distinct
// "watch the wall" stream for the syscall surface, a sibling to the egress
// bridge's "sandbox-egress". An operator greps this to see what an agent tried to
// run by a path other than its tools.
const tapSubject = "sandbox-syscall"

// Offsets into struct seccomp_data (linux/seccomp.h) as the classic-BPF program
// addresses them: the syscall number then the audit arch.
const (
	seccompDataNrOffset   = 0 // offsetof(struct seccomp_data, nr)
	seccompDataArchOffset = 4 // offsetof(struct seccomp_data, arch)
)

// Classic-BPF opcodes used to hand-assemble the seccomp filter. Defined locally
// (rather than leaning on x/sys/unix exporting them) so the filter is fully
// self-contained; the values are the stable cBPF encoding.
const (
	bpfLD  = 0x00
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJMP = 0x05
	bpfJEQ = 0x10
	bpfK   = 0x00
	bpfRET = 0x06
)

// seccompData mirrors struct seccomp_data: the syscall context the kernel copies
// into each notification (and that the BPF program reads).
type seccompData struct {
	Nr                 int32
	Arch               uint32
	InstructionPointer uint64
	Args               [6]uint64
}

// seccompNotif mirrors struct seccomp_notif: one pending notification read from
// the listener fd via SECCOMP_IOCTL_NOTIF_RECV. Its size is ABI-stable and is
// encoded in the RECV ioctl request number, so the layout must match exactly
// (80 bytes on every arch).
type seccompNotif struct {
	ID    uint64
	Pid   uint32
	Flags uint32
	Data  seccompData
}

// seccompNotifResp mirrors struct seccomp_notif_resp: the supervisor's reply
// written with SECCOMP_IOCTL_NOTIF_SEND. For this tap Flags is always
// SECCOMP_USER_NOTIF_FLAG_CONTINUE and Val/Error stay zero — record-then-allow,
// never a synthesized return value (24 bytes).
type seccompNotifResp struct {
	ID    uint64
	Val   int64
	Error int32
	Flags uint32
}

// -------- shim side (runs confined, pre-execve, on the pinned thread) --------

// installSyscallTap installs the seccomp user-notify filter on the calling
// (pinned) thread and hands its notify fd to the parent supervisor, blocking on
// the parent's readiness ack. It is the shim's tap step (ShimMain), a sibling to
// establishEgress/applyLandlock. Fail-closed: any error is returned so ShimMain
// refuses to exec the agent rather than run it unobserved.
func installSyscallTap(sockFD int) error {
	// NO_NEW_PRIVS is a precondition of an unprivileged SECCOMP_SET_MODE_FILTER.
	// applyLandlock also sets it; setting it here (idempotent) means the tap
	// composes whether it runs before or after Landlock.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs for syscall tap: %w", err)
	}

	prog := buildTapFilter()
	fprog := &unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	notifyFD, err := seccompNewListener(fprog)
	runtime.KeepAlive(prog) // the kernel copies prog during the syscall; keep it live until then
	if err != nil {
		return fmt.Errorf("install seccomp user-notify filter: %w", err)
	}

	if err := sendNotifyFD(sockFD, notifyFD); err != nil {
		unix.Close(notifyFD)
		return err
	}
	unix.Close(notifyFD) // the parent holds it now; keep it out of the agent's reach
	unix.Close(sockFD)   // do not leak the control socket into the exec'd agent
	return nil
}

// buildTapFilter assembles the classic-BPF seccomp program: return
// SECCOMP_RET_USER_NOTIF for the enumerated tapped set (execve + execveat) and
// SECCOMP_RET_ALLOW for everything else. The set is deliberately tiny — the
// blueprint is explicit that enumeration completeness is a trap; this is a tap,
// not an enforcer — and these two answer "what did the agent try to run", the
// one bypass surface the Landlock/netns floor cannot observe (a postinstall that
// spawns a downloaded helper), while staying low-frequency so the per-syscall
// round-trip is paid rarely. openat/connect are intentionally excluded: openat is
// far too hot to tap for telemetry, and connect is largely redundant with the
// netns floor + egress bridge (slice 4) which already deny/log the network surface.
func buildTapFilter() []unix.SockFilter {
	tapped := []uint32{uint32(unix.SYS_EXECVE), uint32(unix.SYS_EXECVEAT)}

	// Optional native-arch guard: when tapAuditArch is known, tap only native-ABI
	// syscalls so a compat ABI (whose execve nr differs) cannot masquerade. On an
	// arch we carry no token for (tapAuditArch == 0) the guard is omitted; a
	// spurious tap is harmless because the response is always CONTINUE.
	prog := make([]unix.SockFilter, 0, len(tapped)+6)
	if tapAuditArch != 0 {
		// [0] A = arch; [1] if A==native skip 1 (to LD nr) else [2] ALLOW.
		prog = append(prog,
			bpfStmt(bpfLD|bpfW|bpfABS, seccompDataArchOffset),
			bpfJump(bpfJMP|bpfJEQ|bpfK, tapAuditArch, 1, 0),
			bpfStmt(bpfRET|bpfK, uint32(unix.SECCOMP_RET_ALLOW)),
		)
	}

	// LD nr, then one JEQ per tapped syscall jumping to the trailing NOTIFY, then a
	// default ALLOW, then NOTIFY. jt for each JEQ is the distance to NOTIFY.
	prog = append(prog, bpfStmt(bpfLD|bpfW|bpfABS, seccompDataNrOffset))
	notifyIdx := len(prog) + len(tapped) + 1 // index of the RET USER_NOTIF instruction
	for _, nr := range tapped {
		idx := len(prog)
		prog = append(prog, bpfJump(bpfJMP|bpfJEQ|bpfK, nr, uint8(notifyIdx-idx-1), 0))
	}
	prog = append(prog,
		bpfStmt(bpfRET|bpfK, uint32(unix.SECCOMP_RET_ALLOW)),      // default: allow
		bpfStmt(bpfRET|bpfK, uint32(unix.SECCOMP_RET_USER_NOTIF)), // NOTIFY
	)
	return prog
}

func bpfStmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func bpfJump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

// seccompNewListener installs the filter with SECCOMP_FILTER_FLAG_NEW_LISTENER and
// returns the resulting user-notify fd. The filter is inherited across execve under
// NO_NEW_PRIVS, so it carries into the confined agent.
func seccompNewListener(prog *unix.SockFprog) (int, error) {
	fd, _, e := unix.Syscall(unix.SYS_SECCOMP,
		uintptr(unix.SECCOMP_SET_MODE_FILTER),
		uintptr(unix.SECCOMP_FILTER_FLAG_NEW_LISTENER),
		uintptr(unsafe.Pointer(prog)))
	if e != 0 {
		return -1, e
	}
	return int(fd), nil
}

// sendNotifyFD passes notifyFD to the parent as an SCM_RIGHTS ancillary message
// over the inherited control socket, then blocks on a one-byte readiness ack — the
// handshake that guarantees the supervisor is looping on NOTIF_RECV before the
// shim proceeds to its own (tapped) execve. A closed socket surfaces as EOF and
// fails the shim closed. Mirrors sendTunFD.
func sendNotifyFD(sockFD, notifyFD int) error {
	if err := unix.Sendmsg(sockFD, []byte{'N'}, unix.UnixRights(notifyFD), nil, 0); err != nil {
		return fmt.Errorf("hand seccomp notify fd to parent: %w", err)
	}
	ack := make([]byte, 1)
	n, err := unix.Read(sockFD, ack)
	if err != nil {
		return fmt.Errorf("await syscall-tap readiness: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("syscall-tap parent closed before readiness")
	}
	return nil
}

// -------- parent side (runs in the host/supervisor process) --------

// seccompUserNotifSupported reports whether the kernel knows the
// SECCOMP_RET_USER_NOTIF action — the precondition for the tap. It is a cheap,
// non-contaminating probe (SECCOMP_GET_ACTION_AVAIL installs no filter and needs
// no NO_NEW_PRIVS), so applyIsolation can fail closed up front on a kernel too old
// for the tap. GET_ACTION_AVAIL for USER_NOTIF succeeds only on kernels that also
// have SECCOMP_FILTER_FLAG_NEW_LISTENER (both landed together, ~5.0).
func seccompUserNotifSupported() bool {
	action := uint32(unix.SECCOMP_RET_USER_NOTIF)
	_, _, e := unix.Syscall(unix.SYS_SECCOMP,
		uintptr(unix.SECCOMP_GET_ACTION_AVAIL), 0, uintptr(unsafe.Pointer(&action)))
	return e == 0
}

// tapSupervisor is the parent side of the tap: it owns the control socket, receives
// the notify fd, and serves the RECV→record→CONTINUE loop for the life of ctx. One
// supervisor serves one agent.
type tapSupervisor struct {
	ctx     context.Context
	tracker libtracker.ActivityTracker
	conn    *net.UnixConn // parent end of the shim control socket (fd transport + readiness)
}

// setupSyscallTap wires the supervisor for a command whose spec opted into the tap.
// It creates the parent↔shim control socket, hands the child end to the command as
// an inherited fd, launches the supervisor goroutine, and returns the fd number the
// shim will find the socket at (to record in the plan). It appends exactly one entry
// to cmd.ExtraFiles; the returned fd number accounts for anything already there
// (e.g. an egress slot), so the two compose. Errors wrap ErrIsolation and leave the
// command unstarted. Mirrors setupEgress.
func setupSyscallTap(ctx context.Context, cmd *exec.Cmd, tracker libtracker.ActivityTracker) (int, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("%w: syscall-tap control socketpair: %v", ErrIsolation, err)
	}
	parentFD, childFD := pair[0], pair[1]

	childFile := os.NewFile(uintptr(childFD), "contenox-tap-shim")
	childFDNum := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, childFile)

	parentFile := os.NewFile(uintptr(parentFD), "contenox-tap-parent")
	conn, err := net.FileConn(parentFile)
	parentFile.Close() // net.FileConn dups the fd; drop our original
	if err != nil {
		return 0, fmt.Errorf("%w: syscall-tap control conn: %v", ErrIsolation, err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return 0, fmt.Errorf("%w: syscall-tap control conn is not unix", ErrIsolation)
	}

	s := &tapSupervisor{ctx: ctx, tracker: tracker, conn: uc}
	go s.run()
	return childFDNum, nil
}

// run is the supervisor lifecycle: park for the notify fd, ack readiness, serve
// until ctx is done, tear down. A watcher closes the control socket on ctx
// cancellation so a parked receive unblocks even if the process was never started.
// Mirrors egressBridge.run.
func (s *tapSupervisor) run() {
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-s.ctx.Done():
			s.conn.Close() // unblock a parked ReadMsgUnix / drop the control socket
		case <-watchDone:
		}
	}()

	notifyFD, err := s.recvNotifyFD()
	if err != nil {
		s.conn.Close()
		return
	}

	// Signal the shim that we are about to serve; it then proceeds to (Landlock +)
	// execve. A failed write means the shim is gone — tear down.
	if _, werr := s.conn.Write([]byte{'R'}); werr != nil {
		unix.Close(notifyFD)
		s.conn.Close()
		return
	}

	s.serve(notifyFD)

	unix.Close(notifyFD)
	s.conn.Close()
}

// recvNotifyFD blocks until the shim sends the seccomp-notify fd as SCM_RIGHTS
// ancillary data, then extracts it — close-on-exec (recvOneFD receives with
// MSG_CMSG_CLOEXEC), so the long-lived parent cannot leak this agent's live
// notify fd into a later-spawned sibling that could race its telemetry. Mirrors
// egressBridge.recvTunFD.
func (s *tapSupervisor) recvNotifyFD() (int, error) {
	return recvOneFD(s.conn, "syscall-tap")
}

// serve is the record-then-allow loop. It polls the notify fd (made non-blocking)
// alongside an eventfd that a ctx watcher fires on cancellation, so teardown is
// immediate; if the eventfd cannot be created it degrades to a bounded poll timeout
// that re-checks ctx. It returns on ctx cancellation, on the target going away
// (POLLHUP/POLLERR — the last process holding the filter exited), or on a fatal fd
// error.
func (s *tapSupervisor) serve(notifyFD int) {
	_ = unix.SetNonblock(notifyFD, true)

	cancelFD := -1
	if fd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK); err == nil {
		cancelFD = fd
		defer unix.Close(cancelFD)
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-s.ctx.Done():
				var b [8]byte
				binary.NativeEndian.PutUint64(b[:], 1)
				_, _ = unix.Write(cancelFD, b[:])
			case <-stop:
			}
		}()
	}

	timeout := -1
	if cancelFD < 0 {
		timeout = 250 // ms; no wake fd, so re-check ctx on a bounded tick
	}

	for {
		if cancelFD < 0 && s.ctx.Err() != nil {
			return
		}
		pfds := []unix.PollFd{{Fd: int32(notifyFD), Events: unix.POLLIN}}
		if cancelFD >= 0 {
			pfds = append(pfds, unix.PollFd{Fd: int32(cancelFD), Events: unix.POLLIN})
		}
		n, err := unix.Poll(pfds, timeout)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if n == 0 {
			continue // timeout tick (fallback path)
		}
		if cancelFD >= 0 && pfds[1].Revents != 0 {
			return // ctx canceled
		}
		if pfds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return // target gone / fd closed
		}
		if pfds[0].Revents&unix.POLLIN != 0 {
			if !s.handleNotification(notifyFD) {
				return
			}
		}
	}
}

// handleNotification services one notification: RECV it, RECORD a libtracker event,
// then always SEND CONTINUE. It returns false only on a fatal condition that should
// end the loop. It makes NO decision from the args — record-then-allow, always.
func (s *tapSupervisor) handleNotification(notifyFD int) bool {
	var notif seccompNotif
	if err := notifRecv(notifyFD, &notif); err != nil {
		switch err {
		case unix.EINTR, unix.EAGAIN, unix.ENOENT:
			// interrupted, nothing ready, or the notification was canceled before we
			// read it (target killed): nothing to do, keep serving.
			return true
		default:
			return false // fd closed / fatal
		}
	}

	name, tapped := tappedSyscallName(notif.Data.Nr)
	if !tapped {
		// Should not happen (the filter only notifies the tapped set) but stays
		// legible if the set ever grows without a name mapping.
		name = "syscall-" + strconv.Itoa(int(notif.Data.Nr))
	}
	path := s.readPathArg(notifyFD, &notif, name)

	// The libtracker event: Start(op, subject, first-kv=syscall name) → reportChange
	// → end. reportErr is intentionally never called — the tap OBSERVES; the syscall
	// being denied by Landlock afterward is not the tap's error. The change carries
	// the syscall name, the target pid, and (best-effort) the attempted path.
	_, reportChange, end := s.tracker.Start(s.ctx, "observe", tapSubject,
		"syscall", name, "pid", int(notif.Pid))
	data := map[string]any{"syscall": name, "pid": notif.Pid}
	id := name
	if path != "" {
		data["path"] = path
		id = path
	}
	reportChange(id, data)
	end()

	// Record-then-ALLOW: respond CONTINUE so the kernel runs the real syscall. This
	// is the only response the tap ever sends — no arg-based allow/deny — which is
	// exactly what keeps it TOCTOU-safe (the CONTINUE foot-gun only bites when you
	// CONTINUE on a decision derived from inspected args). Landlock/netns still deny
	// whatever they deny after the syscall proceeds.
	resp := seccompNotifResp{ID: notif.ID, Flags: uint32(unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE)}
	if err := notifSend(notifyFD, &resp); err != nil {
		switch err {
		case unix.ENOENT:
			return true // target already gone / notification canceled — nothing to allow
		default:
			// A SEND failure we cannot recover from — most plausibly CONTINUE being
			// unsupported (pre-5.5, which has NEW_LISTENER but not the CONTINUE flag).
			// We cannot safely let the tapped syscall proceed, so stop serving: run()
			// then closes the notify fd, which makes the kernel fail the target's
			// tapped syscalls with ENOSYS — the shim's execve fails closed rather than
			// the agent running unobserved or blocking forever.
			return false
		}
	}
	return true
}

// tappedSyscallName maps a tapped syscall number to its name for the event. Kept in
// lockstep with buildTapFilter's enumerated set.
func tappedSyscallName(nr int32) (string, bool) {
	switch int(nr) {
	case unix.SYS_EXECVE:
		return "execve", true
	case unix.SYS_EXECVEAT:
		return "execveat", true
	default:
		return "", false
	}
}

// readPathArg best-effort reads the pathname pointer arg for the log string only —
// never a decision. It reads /proc/<pid>/mem at the arg pointer, then validates the
// notification is still live (SECCOMP_IOCTL_NOTIF_ID_VALID): if valid, the target is
// still blocked on THIS syscall so notif.Pid still names it and the bytes are
// trustworthy; if not (pid reuse / canceled notif), the read is discarded. An
// unreadable pointer just yields "" — the event is still emitted with the syscall
// name.
func (s *tapSupervisor) readPathArg(notifyFD int, notif *seccompNotif, name string) string {
	var ptr uint64
	switch name {
	case "execve":
		ptr = notif.Data.Args[0] // execve(const char *pathname, ...)
	case "execveat":
		ptr = notif.Data.Args[1] // execveat(int dirfd, const char *pathname, ...)
	default:
		return ""
	}
	if ptr == 0 {
		return ""
	}
	raw := readProcMemString(int(notif.Pid), ptr)
	if !notifIDValid(notifyFD, notif.ID) {
		return "" // stale: do not trust bytes possibly read from a reused pid
	}
	return raw
}

// readProcMemString reads a NUL-terminated C string of at most PATH_MAX bytes from
// the target's address space via /proc/<pid>/mem (a single pread; a short read at a
// mapping edge is fine). Best-effort: the target runs in the host pid namespace
// (no CLONE_NEWPID), so pid is directly valid, and it is a descendant of this
// supervisor, so the read is normally permitted. Any failure yields "".
func readProcMemString(pid int, addr uint64) string {
	fd, err := unix.Open("/proc/"+strconv.Itoa(pid)+"/mem", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return ""
	}
	defer unix.Close(fd)
	buf := make([]byte, 4096) // PATH_MAX
	n, err := unix.Pread(fd, buf, int64(addr))
	if n <= 0 {
		_ = err
		return ""
	}
	buf = buf[:n]
	if i := indexZero(buf); i >= 0 {
		buf = buf[:i]
	}
	return sanitizeLogString(buf)
}

// indexZero returns the offset of the first NUL byte, or -1.
func indexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// sanitizeLogString bounds the length and replaces control bytes so a hostile or
// truncated pathname cannot corrupt the audit log line.
func sanitizeLogString(b []byte) string {
	const max = 512
	if len(b) > max {
		b = b[:max]
	}
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 0x20 || c == 0x7f {
			out[i] = '?'
			continue
		}
		out[i] = c
	}
	return string(out)
}

// notifRecv reads one pending notification. The kernel requires the buffer be
// zeroed before each RECV.
func notifRecv(fd int, notif *seccompNotif) error {
	*notif = seccompNotif{}
	return seccompIoctl(fd, uintptr(unix.SECCOMP_IOCTL_NOTIF_RECV), unsafe.Pointer(notif))
}

// notifSend writes one response.
func notifSend(fd int, resp *seccompNotifResp) error {
	return seccompIoctl(fd, uintptr(unix.SECCOMP_IOCTL_NOTIF_SEND), unsafe.Pointer(resp))
}

// notifIDValid reports whether a notification id is still outstanding (the target
// still blocked on it), guarding the /proc/<pid>/mem read against pid reuse.
func notifIDValid(fd int, id uint64) bool {
	return seccompIoctl(fd, uintptr(unix.SECCOMP_IOCTL_NOTIF_ID_VALID), unsafe.Pointer(&id)) == nil
}

func seccompIoctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}
