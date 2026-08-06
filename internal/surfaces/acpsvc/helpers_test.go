package acpsvc

import (
	"path/filepath"
	"runtime"

	"github.com/contenox/contenox/libacp"
)

// absTestPath returns p as a genuinely OS-absolute path: a bare leading
// separator is not absolute on Windows (filepath.IsAbs needs a drive
// letter there), so fixtures exercising that check need a real root.
func absTestPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	return `C:` + filepath.FromSlash(p)
}

func mockTransportForFS(caps libacp.FileSystemCapabilities) *Transport {
	t := &Transport{
		sessions:        make(map[libacp.SessionID]*sessionEntry),
		contenoxToACPID: make(map[string]libacp.SessionID),
	}
	t.clientCaps = libacp.ClientCapabilities{FS: caps}
	return t
}
