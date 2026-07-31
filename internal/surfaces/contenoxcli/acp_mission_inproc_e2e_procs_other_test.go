//go:build !linux

package contenoxcli

// childPIDs and pidAlive have no portable implementation: the real reading
// comes from /proc (Linux-only). These stubs exist only so this file's
// callers compile on other platforms; TestSystem_ACPMissionInProcess skips
// itself before ever calling them (see the GOOS guard next to its
// testing.Short() skip).

func childPIDs(parentPid int) []int { return nil }

func pidAlive(pid int) bool { return false }
