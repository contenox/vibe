//go:build windows

package shellsession

import "errors"

// ptySession is unimplemented on Windows: the shell-session surface is a
// POSIX-PTY feature and serve's chat workspace is a Unix concept. A separate
// ConPTY-backed implementation (mirroring terminalservice's create_windows.go)
// would be the future path; until then shell sessions are absent on Windows,
// which the manager reports as ErrUnsupported.
type ptySession struct{}

// ErrUnsupported is returned by startPTY on platforms without a PTY backend.
var ErrUnsupported = errors.New("shellsession: shell sessions are not supported on this platform")

func startPTY(cwd, shell string, scrub func([]string) []string) (*ptySession, error) {
	return nil, ErrUnsupported
}

func (p *ptySession) Read(b []byte) (int, error)  { return 0, ErrUnsupported }
func (p *ptySession) Write(b []byte) (int, error) { return 0, ErrUnsupported }
func (p *ptySession) close()                      {}
func (p *ptySession) wait()                       {}

func defaultShell() string { return "" }
