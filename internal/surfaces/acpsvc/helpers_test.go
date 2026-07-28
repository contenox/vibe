package acpsvc

import (
	"github.com/contenox/contenox/libacp"
)

func mockTransportForFS(caps libacp.FileSystemCapabilities) *Transport {
	t := &Transport{
		sessions:        make(map[libacp.SessionID]*sessionEntry),
		contenoxToACPID: make(map[string]libacp.SessionID),
	}
	t.clientCaps = libacp.ClientCapabilities{FS: caps}
	return t
}
