//go:build windows

package terminalservice

func (s *service) resizeLocalPTY(string, int, int) {}
