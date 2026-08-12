//go:build !linux

package contenoxcli

func childPIDs(parentPid int) []int { return nil }

func pidAlive(pid int) bool { return false }
