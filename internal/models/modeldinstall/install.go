package modeldinstall

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	runtimeversion "github.com/contenox/contenox/internal/version"
)

// sumCheckTimeout bounds the .sha256 availability probe so a hung network reads
// as a transient failure (source-build fallback) rather than blocking setup.
const sumCheckTimeout = 30 * time.Second

// Options configures EnsureInstalled. The zero value is valid: every field
// defaults to the released behavior. Non-default fields exist for tests and dev
// builds; end users never set them.
type Options struct {
	BaseURL       string       // default DefaultBaseURL
	ClientVersion string       // default runtime version.Get(); used only for User-Agent
	DataRoot      string       // install root parent; default ~/.contenox
	GOOS          string       // default runtime.GOOS
	GOARCH        string       // default runtime.GOARCH
	HTTPClient    *http.Client // default a no-overall-timeout client
	Progress      io.Writer    // download progress sink; default io.Discard
}

// Result describes a successful install (or a compatible pre-existing install).
type Result struct {
	LauncherPath     string   // absolute path to the runnable modeld launcher
	Version          string   // version the installed binary reports
	Protocol         int      // transport protocol the installed binary reports
	Platform         string   // e.g. "linux-amd64"
	Backends         []string // compiled backends the binary reports
	AlreadyInstalled bool     // true when a compatible binary was already present
}

type client struct {
	baseURL       string
	clientVersion string
	dataRoot      string
	goos          string
	goarch        string
	http          *http.Client
	progress      io.Writer
}

func newClient(opts Options) (*client, error) {
	c := &client{
		baseURL:       firstNonEmpty(opts.BaseURL, DefaultBaseURL),
		clientVersion: strings.TrimSpace(firstNonEmpty(opts.ClientVersion, runtimeversion.Get())),
		dataRoot:      opts.DataRoot,
		goos:          firstNonEmpty(opts.GOOS, runtime.GOOS),
		goarch:        firstNonEmpty(opts.GOARCH, runtime.GOARCH),
		http:          opts.HTTPClient,
		progress:      opts.Progress,
	}
	if c.dataRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("modeld setup: resolve data root: %w", err)
		}
		c.dataRoot = filepath.Join(home, ".contenox")
	}
	if c.http == nil {
		// No overall client Timeout: a large archive download must not be cut
		// mid-stream. Per-step bounds come from the request context.
		c.http = &http.Client{}
	}
	if c.progress == nil {
		c.progress = io.Discard
	}
	return c, nil
}

// EnsureInstalled makes a compatible modeld available for the selected provider
// ("llama" or "openvino"): it reuses a compatible managed install, otherwise it
// resolves the newest compatible stable build from the public index, downloads,
// verifies, safely extracts, validates with `modeld version --json`, atomically
// installs into ~/.contenox/modeld/<modeld-version>/<platform>/, and writes the
// current pointer. Errors are typed (see errors.go) so the caller can fall back
// to source-build for soft failures and surface a checksum mismatch as a hard
// failure.
func EnsureInstalled(ctx context.Context, provider string, opts Options) (Result, error) {
	c, err := newClient(opts)
	if err != nil {
		return Result{}, err
	}
	return c.ensure(ctx, provider)
}

func (c *client) ensure(ctx context.Context, provider string) (Result, error) {
	platform := Platform(c.goos, c.goarch)
	if archiveExt(c.goos) == "" {
		return Result{}, ErrUnsupportedPlatform
	}

	if inst, err := FindCompatibleInstall(ctx, c.dataRoot, c.goos, c.goarch, provider); err == nil {
		return Result{
			LauncherPath:     inst.LauncherPath,
			Version:          inst.Version,
			Protocol:         inst.Protocol,
			Platform:         inst.Platform,
			Backends:         inst.Backends,
			AlreadyInstalled: true,
		}, nil
	}

	idx, err := c.fetchIndex(ctx)
	if err != nil {
		return Result{}, err
	}
	build, err := selectBuild(idx, platform, provider)
	if err != nil {
		return Result{}, err
	}
	art, err := artifactFromBuild(c.baseURL, build, c.goos)
	if err != nil {
		return Result{}, err
	}
	fmt.Fprintf(c.progress, "Selected modeld %s (protocol %d, backends: %s)\n", art.version, art.protocol, strings.Join(art.backends, ", "))

	installDir := ManagedInstallDir(c.dataRoot, art.version, c.goos, c.goarch)
	launcher := filepath.Join(installDir, LauncherName(c.goos))

	// 1. Reuse a compatible existing install rather than re-downloading.
	if info, perr := c.probeBinary(ctx, launcher); perr == nil {
		if checkCapability(info, provider) == nil {
			if err := WriteCurrentPointer(c.dataRoot, installDir); err != nil {
				return Result{}, err
			}
			return Result{
				LauncherPath:     launcher,
				Version:          info.Version,
				Protocol:         info.Protocol,
				Platform:         art.platform,
				Backends:         info.Backends,
				AlreadyInstalled: true,
			}, nil
		}
	}

	// 2. Fetch the selected checksum from the index-provided path.
	sumCtx, cancel := context.WithTimeout(ctx, sumCheckTimeout)
	defer cancel()
	sumText, err := c.getSmallText(sumCtx, art.sumURL)
	if err != nil {
		return Result{}, err
	}
	want, err := parseSHA256(sumText)
	if err != nil {
		return Result{}, err
	}

	// 3. Download into a staging area on the same filesystem as the install dir,
	// so the final move is an atomic rename.
	stagingParent := filepath.Join(c.dataRoot, "modeld", ".staging")
	if err := os.MkdirAll(stagingParent, 0o755); err != nil {
		return Result{}, err
	}
	fmt.Fprintf(c.progress, "Downloading %s...\n", art.name)
	tmpArchive, err := c.downloadToTemp(ctx, art.archiveURL, stagingParent, "modeld-*.download")
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(tmpArchive)

	// 4. Verify before extracting. A mismatch is a hard failure.
	if err := verifyChecksum(tmpArchive, want); err != nil {
		return Result{}, err
	}
	fmt.Fprintf(c.progress, "Verified checksum %s\n", want)

	// 5. Extract into a fresh staging dir.
	staging, err := os.MkdirTemp(stagingParent, fmt.Sprintf("%s-%s-*", art.version, art.platform))
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(staging)
	if strings.HasSuffix(art.name, ".zip") {
		err = extractZip(tmpArchive, staging)
	} else {
		err = extractTarGz(tmpArchive, staging)
	}
	if err != nil {
		return Result{}, err
	}
	extractedRoot, err := resolveExtractedRoot(staging, art)
	if err != nil {
		return Result{}, err
	}

	// 6. Validate the extracted binary before moving it into place.
	stagedLauncher := filepath.Join(extractedRoot, LauncherName(c.goos))
	info, err := c.probeBinary(ctx, stagedLauncher)
	if err != nil {
		return Result{}, err
	}
	if err := checkCapability(info, provider); err != nil {
		return Result{}, err
	}

	// 7. Atomically install: rename the extracted dir over the version/platform dir.
	if err := os.MkdirAll(filepath.Dir(installDir), 0o755); err != nil {
		return Result{}, err
	}
	if err := replaceDir(extractedRoot, installDir); err != nil {
		return Result{}, err
	}
	if err := WriteCurrentPointer(c.dataRoot, installDir); err != nil {
		return Result{}, err
	}
	fmt.Fprintf(c.progress, "Installed modeld to %s\n", launcher)
	return Result{
		LauncherPath: launcher,
		Version:      info.Version,
		Protocol:     info.Protocol,
		Platform:     art.platform,
		Backends:     info.Backends,
	}, nil
}

// resolveExtractedRoot returns the directory that holds the modeld launcher after
// extraction. The release packs a single top-level modeld-<version>-<platform>/
// directory; if a future archive is flat, fall back to the staging dir itself.
func resolveExtractedRoot(staging string, a artifact) (string, error) {
	nested := filepath.Join(staging, a.topLevelDir())
	if dirExists(nested) {
		return nested, nil
	}
	if fileExists(filepath.Join(staging, "modeld")) || fileExists(filepath.Join(staging, "modeld.cmd")) {
		return staging, nil
	}
	return "", fmt.Errorf("modeld setup: archive did not contain %s/", a.topLevelDir())
}

// replaceDir moves src onto dst, replacing any existing install. On Windows the
// remove can fail if a modeld process is using the old binary; in that case the
// existing install is left intact and the caller is told to stop modeld first.
func replaceDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("modeld setup: replace install (is modeld running? stop it and retry): %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("modeld setup: install rename: %w", err)
	}
	return nil
}

// ManagedInstallDir is the per-user install directory for a modeld release:
// <dataRoot>/modeld/<version>/<platform>. It is the canonical layout shared by
// the installer and the binary-discovery probe.
func ManagedInstallDir(dataRoot, version, goos, goarch string) string {
	return filepath.Join(dataRoot, "modeld", version, Platform(goos, goarch))
}

// ManagedLauncherPath is the runnable launcher inside ManagedInstallDir.
func ManagedLauncherPath(dataRoot, version, goos, goarch string) string {
	return filepath.Join(ManagedInstallDir(dataRoot, version, goos, goarch), LauncherName(goos))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
