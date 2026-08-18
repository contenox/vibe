package version

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

//go:embed version.txt
var versionFile string

var version string

func Get() string {
	return version
}

// Provenance is the VCS state the Go toolchain stamped into the binary. The
// zero value means no VCS metadata was embedded (go test, or a build outside
// a repository); only Dirty separates a working-tree build from the release
// version.txt claims.
type Provenance struct {
	Revision string
	Dirty    bool
	Time     string
}

// String renders "revision <rev> (working tree modified), built <time>",
// omitting absent parts; empty for the zero value.
func (p Provenance) String() string {
	var parts []string
	switch {
	case p.Revision != "" && p.Dirty:
		parts = append(parts, "revision "+p.Revision+" (working tree modified)")
	case p.Revision != "":
		parts = append(parts, "revision "+p.Revision)
	case p.Dirty:
		parts = append(parts, "working tree modified")
	}
	if p.Time != "" {
		parts = append(parts, "built "+p.Time)
	}
	return strings.Join(parts, ", ")
}

// GetProvenance reads vcs.revision/vcs.modified/vcs.time from the running
// binary's build info.
func GetProvenance() Provenance {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Provenance{}
	}
	return provenanceFromSettings(info.Settings)
}

func provenanceFromSettings(settings []debug.BuildSetting) Provenance {
	var p Provenance
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			p.Revision = s.Value
		case "vcs.modified":
			p.Dirty = s.Value == "true"
		case "vcs.time":
			p.Time = s.Value
		}
	}
	return p
}

func init() {
	version = strings.TrimSpace(versionFile)
	if version == "" {
		version = "unknown"
	}
}
