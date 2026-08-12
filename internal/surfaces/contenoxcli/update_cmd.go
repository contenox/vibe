package contenoxcli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/contenox/contenox/internal/services/updatecheck"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

const releasesURL = "https://github.com/contenox/contenox/releases"

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update contenox to the latest release.",
	Long: `Download and install the latest contenox release binary from GitHub.

If already on the latest version, nothing is downloaded.

  contenox update          check and install if a newer version is available
  contenox update check    print version info without installing

To disable automatic update notifications set the opt-out flag:
  contenox config set update-check false`,
	RunE: runUpdateInstall,
}

var updateCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check if a newer version is available without installing.",
	Long: `Compare the current version against the latest GitHub release and report
whether an update is available, without downloading or installing anything.
Does nothing if update checks have been disabled via 'contenox config set
update-check false'.`,
	RunE: runUpdateCheck,
}

func init() {
	updateCmd.AddCommand(updateCheckCmd)
}

func runUpdateCheck(cmd *cobra.Command, _ []string) error {
	ctx := libtracker.WithNewRequestID(context.Background())
	current := CLIVersion()

	contenoxDir, err := globalContenoxDir()
	if err != nil {
		return fmt.Errorf("could not determine contenox dir: %w", err)
	}

	if isUpdateCheckDisabled(cmd, ctx) {
		fmt.Fprintln(cmd.OutOrStdout(), "Update checks are disabled (update-check = false).")
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Current version:  %s\n", current)
	fmt.Fprintln(cmd.OutOrStdout(), "Checking for updates...")

	latest, available, err := updatecheck.IsAvailable(ctx, current, contenoxDir)
	if err != nil {
		return fmt.Errorf("could not check for updates: %w", err)
	}

	if available {
		fmt.Fprintf(cmd.OutOrStdout(), "Update available: %s  →  run `contenox update` to install.\n", latest)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Already on the latest version (%s).\n", current)
	}
	return nil
}

func runUpdateInstall(cmd *cobra.Command, _ []string) error {
	ctx := libtracker.WithNewRequestID(context.Background())
	current := CLIVersion()

	contenoxDir, err := globalContenoxDir()
	if err != nil {
		return fmt.Errorf("could not determine contenox dir: %w", err)
	}

	if isUpdateCheckDisabled(cmd, ctx) {
		fmt.Fprintln(cmd.ErrOrStderr(), "Update checks are disabled (update-check = false). Run `contenox config set update-check true` to re-enable.")
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Current version: %s\n", current)
	fmt.Fprintln(cmd.OutOrStdout(), "Checking for updates...")

	latest, available, err := updatecheck.IsAvailable(ctx, current, contenoxDir)
	if err != nil {
		return fmt.Errorf("could not check for updates: %w", err)
	}

	if !available {
		fmt.Fprintf(cmd.OutOrStdout(), "Already on the latest version (%s).\n", current)
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Downloading %s...\n", latest)
	return downloadAndReplace(ctx, cmd, latest)
}

// isUpdateCheckDisabled returns true when the user has opted out via config.
// DB errors are ignored — an absent DB means fresh install, updates are enabled.
func isUpdateCheckDisabled(cmd *cobra.Command, ctx context.Context) bool {
	db, store, err := openConfigDB(cmd)
	if err != nil {
		return false
	}
	defer db.Close()
	val, _ := getConfigKV(ctx, store, "update-check")
	return val == "false"
}

func downloadAndReplace(ctx context.Context, cmd *cobra.Command, tag string) error {
	ext := ""
	if goruntime.GOOS == "windows" {
		ext = ".exe"
	}
	asset := fmt.Sprintf("contenox-%s-%s%s", goruntime.GOOS, goruntime.GOARCH, ext)
	url := fmt.Sprintf("%s/download/%s/%s", releasesURL, tag, asset)

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine current binary path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("could not resolve binary symlinks: %w", err)
	}
	installDir := filepath.Dir(exe)

	tmp, err := os.CreateTemp(installDir, ".contenox-update-*"+ext)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("cannot write to %s, which is required to install the update.\n\n%s\n\nOr download it manually from %s", installDir, elevateHint(), releasesURL)
		}
		return fmt.Errorf("could not create temp file for download: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath) // no-op after a successful rename
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "contenox-selfupdate")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: server returned %d for %s\nPlease download manually from %s", resp.StatusCode, url, releasesURL)
	}

	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), resp.Body); err != nil {
		return fmt.Errorf("download interrupted: %w", err)
	}

	expected, err := fetchExpectedSum(ctx, tag, asset)
	if err != nil {
		return err
	}
	if actual := hex.EncodeToString(sum.Sum(nil)); actual != expected {
		return fmt.Errorf("CHECKSUM MISMATCH for %s.\n  expected: %s\n  actual:   %s\n\nThe downloaded file does not match the published release. This could be a\ncorrupted download or a tampered artifact. Nothing was installed.\nReport it at https://github.com/contenox/contenox/security/advisories", asset, expected, actual)
	}

	if err := tmp.Chmod(0o755); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("could not finalize downloaded file: %w", err)
	}

	if goruntime.GOOS == "windows" {
		// Windows cannot replace a running .exe; rename it aside first.
		old := exe + ".old"
		_ = os.Remove(old) // discard any leftover from a previous update attempt
		if err := os.Rename(exe, old); err != nil {
			return fmt.Errorf("could not move existing binary: %w\n\n%s\n\nOr download it manually from %s", err, elevateHint(), releasesURL)
		}
	}

	if err := os.Rename(tmpPath, exe); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("could not install update to %s: %w\n\n%s\n\nOr download it manually from %s", exe, err, elevateHint(), releasesURL)
		}
		return fmt.Errorf("could not install update to %s: %w", exe, err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "contenox updated to %s — restart any running instances.\n", tag)
	return nil
}

func elevateHint() string {
	if goruntime.GOOS == "windows" {
		return "Re-run `contenox update` from an Administrator prompt."
	}
	return "Re-run with elevated privileges:\n    sudo contenox update"
}

func fetchExpectedSum(ctx context.Context, tag, asset string) (string, error) {
	url := fmt.Sprintf("%s/download/%s/SHA256SUMS", releasesURL, tag)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "contenox-selfupdate")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not download SHA256SUMS for %s: %w\nRefusing to install an unverified binary.", tag, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("could not download SHA256SUMS for %s: server returned %d.\nRefusing to install an unverified binary.\n  expected: %s\nReleases published before checksums existed do not carry this file. Update to\na newer release, or download and verify manually from %s", tag, resp.StatusCode, url, releasesURL)
	}

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("could not read SHA256SUMS for %s: %w", tag, err)
	}
	return "", fmt.Errorf("SHA256SUMS for %s has no entry for %s.\nRefusing to install an unverified binary.", tag, asset)
}
