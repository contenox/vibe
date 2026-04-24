package version

import (
	_ "embed"
	"strings"
)

//go:embed version.txt
var versionFile string

var version string

func Get() string {
	return version
}

func init() {
	version = strings.TrimSpace(versionFile)
	if version == "" {
		version = "unknown"
	}
}
