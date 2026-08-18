package contenoxcli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestUnit_Beam_IsItsOwnCommandNotAServeAlias(t *testing.T) {
	var found *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Name() == "beam" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("'contenox beam' must be registered on the root command")
	}
	if found.Hidden {
		t.Fatal("'contenox beam' is the terminal surface, not a hidden alias")
	}
	if acpProfileBeam.host {
		t.Fatal("the beam profile must not run the unattended host")
	}
	if !acpProfileBeam.beam {
		t.Fatal("the beam profile must select the terminal surface")
	}
	if acpProfileServe.beam {
		t.Fatal("'contenox serve' must stay unattended")
	}
	if acpProfileBeam.name == acpProfileServe.name {
		t.Fatalf("beam and serve must not share a log name: both are %q", acpProfileBeam.name)
	}
}

func TestUnit_BeamRoot_DefaultsToTheLaunchDirectory(t *testing.T) {
	cmd := &cobra.Command{Use: "beam"}
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, err := beamRoot(cmd, "/launch/dir")
	if err != nil {
		t.Fatalf("beamRoot: %v", err)
	}
	if got != "/launch/dir" {
		t.Fatalf("expected the launch directory, got %q", got)
	}
}

func TestUnit_BeamRoot_TakesThePositionalPath(t *testing.T) {
	dir := t.TempDir()
	cmd := &cobra.Command{Use: "beam"}
	if err := cmd.ParseFlags([]string{dir}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, err := beamRoot(cmd, "/launch/dir")
	if err != nil {
		t.Fatalf("beamRoot: %v", err)
	}
	if got != filepath.Clean(dir) {
		t.Fatalf("expected %q, got %q", dir, got)
	}
}

func TestUnit_BeamRoot_RefusesANonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := &cobra.Command{Use: "beam"}
	if err := cmd.ParseFlags([]string{file}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if _, err := beamRoot(cmd, "/launch/dir"); err == nil {
		t.Fatal("expected a file to be refused as a workspace root")
	}
}
