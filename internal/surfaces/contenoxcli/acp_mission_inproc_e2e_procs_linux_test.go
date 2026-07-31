//go:build linux

package contenoxcli

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// childPIDs returns the pids of live processes whose parent is parentPid — the
// dispatched unit subprocess(es) an editor spawned. Linux-only (the e2e env),
// read straight from /proc.
func childPIDs(parentPid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, convErr := strconv.Atoi(e.Name())
		if convErr != nil {
			continue
		}
		if ppid, ok := procPPID(pid); ok && ppid == parentPid {
			out = append(out, pid)
		}
	}
	return out
}

// procPPID reads the parent pid from /proc/<pid>/stat. The comm field (field 2)
// can contain spaces and parentheses, so PPID is parsed from AFTER the last ')'.
func procPPID(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+1 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] = state, fields[1] = ppid.
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// pidAlive reports whether pid names a live process (signal 0 probe): ESRCH means
// gone (reaped), any other outcome means it still exists.
func pidAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
