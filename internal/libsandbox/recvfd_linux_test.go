//go:build linux

package libsandbox

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestUnit_recvOneFD_ReceivedFDIsCloexec pins that an fd received over
// SCM_RIGHTS comes back close-on-exec (via recvOneFD's MSG_CMSG_CLOEXEC), so
// the long-lived parent cannot leak one agent's fd into a later-spawned
// sibling; SCM_RIGHTS fds are not cloexec by default, so a plain ReadMsgUnix
// would fail this test.
func TestUnit_recvOneFD_ReceivedFDIsCloexec(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	sendFD, recvFD := pair[0], pair[1]
	defer unix.Close(sendFD)

	recvFile := os.NewFile(uintptr(recvFD), "recv")
	conn, err := net.FileConn(recvFile)
	recvFile.Close() // net.FileConn dups; drop the original
	if err != nil {
		t.Fatalf("FileConn: %v", err)
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("conn is not *net.UnixConn: %T", conn)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	if err := unix.Sendmsg(sendFD, []byte{'x'}, unix.UnixRights(int(r.Fd())), nil, 0); err != nil {
		t.Fatalf("sendmsg: %v", err)
	}

	got, err := recvOneFD(uc, "test")
	if err != nil {
		t.Fatalf("recvOneFD: %v", err)
	}
	defer unix.Close(got)

	flags, err := unix.FcntlInt(uintptr(got), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("F_GETFD: %v", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("received fd is NOT close-on-exec (F_GETFD=%#x): it can leak into a spawned child", flags)
	}
}
