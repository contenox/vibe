package modeldinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var errNoCompatibleInstall = errors.New("no compatible managed modeld install")

// InstalledBinary describes a compatible managed modeld installation found on
// disk.
type InstalledBinary struct {
	LauncherPath string
	InstallDir   string
	Version      string
	Platform     string
	Protocol     int
	Backends     []string
}

// CurrentPointerPath is the cross-platform pointer to the active managed
// install. The file contains an absolute install directory; symlinks are also
// accepted for operator-managed installs.
func CurrentPointerPath(dataRoot string) string {
	return filepath.Join(dataRoot, "modeld", "current")
}

// WriteCurrentPointer atomically points current at installDir.
func WriteCurrentPointer(dataRoot, installDir string) error {
	abs, err := filepath.Abs(installDir)
	if err != nil {
		return fmt.Errorf("modeld setup: resolve current pointer target: %w", err)
	}
	ptr := CurrentPointerPath(dataRoot)
	if err := os.MkdirAll(filepath.Dir(ptr), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(ptr), ".current-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(abs + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, ptr); err != nil {
		return fmt.Errorf("modeld setup: write current pointer: %w", err)
	}
	return nil
}

// CurrentLauncherPath resolves the current pointer to a runnable launcher.
func CurrentLauncherPath(dataRoot, goos string) (string, error) {
	ptr := CurrentPointerPath(dataRoot)
	info, err := os.Lstat(ptr)
	if err != nil {
		return "", err
	}
	var target string
	if info.Mode()&os.ModeSymlink != 0 {
		target, err = os.Readlink(ptr)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(ptr), target)
		}
	} else {
		b, err := os.ReadFile(ptr)
		if err != nil {
			return "", err
		}
		target = strings.TrimSpace(string(b))
	}
	if target == "" {
		return "", fmt.Errorf("modeld setup: empty current pointer")
	}
	if filepath.Base(target) == LauncherName(goos) {
		return target, nil
	}
	return filepath.Join(target, LauncherName(goos)), nil
}

// FindCompatibleInstall finds a managed install compatible with this runtime's
// transport protocol and, when provider is non-empty, with that backend compiled
// in. It checks the current pointer first, then scans all version-keyed installs.
func FindCompatibleInstall(ctx context.Context, dataRoot, goos, goarch, provider string) (InstalledBinary, error) {
	platform := Platform(goos, goarch)
	if launcher, err := CurrentLauncherPath(dataRoot, goos); err == nil {
		if inst, err := probeManagedLauncher(ctx, launcher, platform, provider); err == nil {
			return inst, nil
		}
	}

	root := filepath.Join(dataRoot, "modeld")
	entries, err := os.ReadDir(root)
	if err != nil {
		return InstalledBinary{}, errNoCompatibleInstall
	}
	var candidates []InstalledBinary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == ".staging" {
			continue
		}
		launcher := filepath.Join(root, name, platform, LauncherName(goos))
		if !fileExists(launcher) {
			continue
		}
		inst, err := probeManagedLauncher(ctx, launcher, platform, provider)
		if err != nil {
			continue
		}
		candidates = append(candidates, inst)
	}
	if len(candidates) == 0 {
		return InstalledBinary{}, errNoCompatibleInstall
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		cmp := compareVersions(candidates[i].Version, candidates[j].Version)
		if cmp == 0 {
			return candidates[i].LauncherPath > candidates[j].LauncherPath
		}
		return cmp > 0
	})
	return candidates[0], nil
}

func probeManagedLauncher(ctx context.Context, launcher, platform, provider string) (InstalledBinary, error) {
	info, err := ProbeBinary(ctx, launcher)
	if err != nil {
		return InstalledBinary{}, err
	}
	if err := checkCapability(info, provider); err != nil {
		return InstalledBinary{}, err
	}
	return InstalledBinary{
		LauncherPath: launcher,
		InstallDir:   filepath.Dir(launcher),
		Version:      info.Version,
		Platform:     platform,
		Protocol:     info.Protocol,
		Backends:     append([]string(nil), info.Backends...),
	}, nil
}
