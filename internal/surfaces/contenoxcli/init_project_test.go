package contenoxcli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/services/project"
)

// TestUnit_InitProject_ForcesLocalMarker asserts `init --project` gives a nested child directory its own local marker and workspace id, rather than reusing an ancestor's.
func TestUnit_InitProject_ForcesLocalMarker(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "contenox-init-project-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// parent/ carries a project marker; child/ is nested under it.
	parentDir := filepath.Join(tempDir, "parent")
	childDir := filepath.Join(parentDir, "sub1", "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("Failed to create child dir: %v", err)
	}

	parentMarker, err := project.EnsureInProjectRoot(parentDir, "parent-proj")
	if err != nil {
		t.Fatalf("Failed to seed parent marker: %v", err)
	}
	if parentMarker.ID == "" {
		t.Fatal("expected parent marker to carry a non-empty id")
	}

	// Sanity: WITHOUT --project, resolving from the child walks up and reuses the
	// parent's .contenox — this is exactly the behavior --project must bypass.
	t.Chdir(childDir)
	resolved, err := ResolveContenoxDir(nil)
	if err != nil {
		t.Fatalf("ResolveContenoxDir: %v", err)
	}
	if want := filepath.Join(parentDir, project.ContenoxDirName); resolved != want {
		t.Fatalf("walk-up should reuse the ancestor marker: want %q, got %q", want, resolved)
	}

	// The --project path: resolve a LOCAL marker in cwd and write it. This mirrors
	// runInitCmd's --project branch (resolveProjectInit + EnsureInContenoxDir), the
	// only marker-writing step of RunInit exercised without its heavy scaffolding.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	contenoxDir, projectName := resolveProjectInit(cwd, "")

	if want := filepath.Join(childDir, project.ContenoxDirName); contenoxDir != want {
		t.Fatalf("--project must target the local dir: want %q, got %q", want, contenoxDir)
	}
	if projectName != filepath.Base(childDir) {
		t.Fatalf("empty --name should default to the dir basename %q, got %q", filepath.Base(childDir), projectName)
	}

	childMarker, err := project.EnsureInContenoxDir(contenoxDir, projectName)
	if err != nil {
		t.Fatalf("EnsureInContenoxDir: %v", err)
	}

	// A NEW local marker file must exist under the child.
	if _, err := os.Stat(filepath.Join(contenoxDir, project.MarkerFileName)); err != nil {
		t.Fatalf("expected a local marker at %s: %v", contenoxDir, err)
	}
	// It must carry a DIFFERENT id than the parent (a distinct project/workspace).
	if childMarker.ID == "" {
		t.Fatal("expected child marker to carry a non-empty id")
	}
	if childMarker.ID == parentMarker.ID {
		t.Fatalf("child marker id %q must differ from parent id %q", childMarker.ID, parentMarker.ID)
	}
	// And its name must be set to the child dir basename.
	if childMarker.Name != filepath.Base(childDir) {
		t.Fatalf("expected child marker name %q, got %q", filepath.Base(childDir), childMarker.Name)
	}

	// The workspace-id resolver must now report the child's OWN id from the child
	// dir, confirming the ancestor marker no longer shadows it.
	if got := ResolveWorkspaceID(contenoxDir); got != childMarker.ID {
		t.Fatalf("ResolveWorkspaceID(child) = %q, want %q", got, childMarker.ID)
	}
}

// TestUnit_ResolveProjectInit_ExplicitNameWins asserts an explicit --name is used verbatim rather than replaced by the directory basename.
func TestUnit_ResolveProjectInit_ExplicitNameWins(t *testing.T) {
	cwd := filepath.Join(string(filepath.Separator), "tmp", "some-dir")
	contenoxDir, projectName := resolveProjectInit(cwd, "My Project")
	if want := filepath.Join(cwd, project.ContenoxDirName); contenoxDir != want {
		t.Fatalf("contenoxDir: want %q, got %q", want, contenoxDir)
	}
	if projectName != "My Project" {
		t.Fatalf("explicit name should win: got %q", projectName)
	}
}
