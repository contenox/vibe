// Package modeldinstall discovers, downloads, verifies, installs, and validates a
// prebuilt modeld package for the current machine. It is the implementation behind
// the `contenox setup` check for the local llama/openvino providers: see
// docs/development/blueprints/modeld/version-decoupling.md.
//
// It speaks plain HTTPS to the public release prefix — no AWS SDK, no bucket
// listing, no credentials. The public index is the availability source of truth;
// selected archives are verified against their checksum before extraction, and
// the extracted binary is validated with `modeld version --json` before it is
// moved into place.
package modeldinstall

import (
	"fmt"
	"path"
	"strings"
)

// DefaultBaseURL is the public modeld release prefix compiled into the released
// CLI. Only the final-package prefix is public; the native dependency prefix
// (modeld-deps/) stays private. Tests inject an alternate via Options.BaseURL.
const DefaultBaseURL = "https://contenox-modeld-artifacts-573643652148.s3.amazonaws.com/modeld"

// Platform is the release platform string, e.g. "linux-amd64".
func Platform(goos, goarch string) string { return goos + "-" + goarch }

// archiveExt returns the published archive extension for goos, or "" if the OS
// has no published package format.
func archiveExt(goos string) string {
	switch goos {
	case "linux", "darwin":
		return ".tar.gz"
	case "windows":
		return ".zip"
	default:
		return ""
	}
}

// LauncherName is the entry-point script inside the package for goos. The launcher
// (not modeld.bin/modeld.exe) is what callers run: it sets the native library
// search path before exec'ing the real binary.
func LauncherName(goos string) string {
	if goos == "windows" {
		return "modeld.cmd"
	}
	return "modeld"
}

// artifact holds the release object selected from the public index.
type artifact struct {
	version    string
	platform   string
	protocol   int
	backends   []string
	name       string
	archiveURL string
	sumURL     string
	size       int64
}

// topLevelDir is the single directory the archive unpacks to:
// modeld-<version>-<platform> (the archive name without its archive extension).
// The release packer tars/zips that directory, so extraction yields exactly one
// entry.
func (a artifact) topLevelDir() string {
	name := a.name
	name = strings.TrimSuffix(name, ".tar.gz")
	name = strings.TrimSuffix(name, ".zip")
	return name
}

func artifactFromBuild(baseURL string, b indexBuild, goos string) (artifact, error) {
	ext := archiveExt(goos)
	if ext == "" {
		return artifact{}, ErrUnsupportedPlatform
	}
	if !strings.HasSuffix(b.Archive, ext) {
		return artifact{}, fmt.Errorf("modeld setup: index archive %q does not match expected %s package", b.Archive, ext)
	}
	if err := validateRelativeObjectPath(b.Archive); err != nil {
		return artifact{}, fmt.Errorf("modeld setup: invalid archive path in index: %w", err)
	}
	if err := validateRelativeObjectPath(b.SHA256); err != nil {
		return artifact{}, fmt.Errorf("modeld setup: invalid checksum path in index: %w", err)
	}
	return artifact{
		version:    strings.TrimSpace(b.Version),
		platform:   b.Platform,
		protocol:   b.Protocol,
		backends:   append([]string(nil), b.Backends...),
		name:       path.Base(b.Archive),
		archiveURL: objectURL(baseURL, b.Archive),
		sumURL:     objectURL(baseURL, b.SHA256),
		size:       b.Size,
	}, nil
}

func objectURL(baseURL, rel string) string {
	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(rel, "/")
}

func validateRelativeObjectPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("%q is absolute", p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%q escapes the release prefix", p)
	}
	return nil
}
