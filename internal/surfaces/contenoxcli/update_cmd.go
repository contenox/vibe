package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/contenox/contenox/internal/services/updatecheck"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

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
	url := fmt.Sprintf("https://github.com/contenox/contenox/releases/download/%s/%s", tag, asset)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: server returned %d for %s\nPlease download manually from https://github.com/contenox/contenox/releases", resp.StatusCode, url)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine current binary path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("could not resolve binary symlinks: %w", err)
	}

	// Write to a temp file in the same directory so the final rename is atomic
	// (same filesystem, no cross-device move).
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".contenox-update-*"+ext)
	if err != nil {
		return fmt.Errorf("could not create temp file for download: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpPath) // no-op after successful rename
	}()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return fmt.Errorf("download interrupted: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}
	tmp.Close()

	if goruntime.GOOS == "windows" {
		// Windows cannot replace a running .exe; rename it aside first.
		old := exe + ".old"
		_ = os.Remove(old) // discard any leftover from a previous update attempt
		if err := os.Rename(exe, old); err != nil {
			return fmt.Errorf("could not move existing binary (try running as Administrator): %w\nDownload manually from https://github.com/contenox/contenox/releases", err)
		}
	}

	if err := os.Rename(tmpPath, exe); err != nil {
		return fmt.Errorf("could not install update to %s: %w", exe, err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "contenox updated to %s — restart any running instances.\n", tag)
	return nil
}
