package contenoxcli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveContenoxDir(t *testing.T) {
	// Create a temporary directory structure for testing.
	tempDir, err := os.MkdirTemp("", "contenox-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir) // cleanup

	// Setup: /tempDir/project/.contenox
	// Setup: /tempDir/project/sub1/sub2
	projectDir := filepath.Join(tempDir, "project")
	sub2Dir := filepath.Join(projectDir, "sub1", "sub2")

	if err := os.MkdirAll(sub2Dir, 0755); err != nil {
		t.Fatalf("Failed to create subdirectories: %v", err)
	}

	contenoxDir := filepath.Join(projectDir, ".contenox")
	if err := os.MkdirAll(contenoxDir, 0755); err != nil {
		t.Fatalf("Failed to create .contenox dir: %v", err)
	}

	// 1. Test from sub2Dir. It should walk up and find it in projectDir.
	err = os.Chdir(sub2Dir)
	if err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	resolvedDir, err := ResolveContenoxDir()
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if resolvedDir != contenoxDir {
		t.Errorf("Expected resolved dir %q, got %q", contenoxDir, resolvedDir)
	}

	// 2. Test from a directory with no .contenox anywhere in the tree.
	noContenoxDir := filepath.Join(tempDir, "otherproject", "sub1")
	if err := os.MkdirAll(noContenoxDir, 0755); err != nil {
		t.Fatalf("Failed to create no-contenox subdirectories: %v", err)
	}

	err = os.Chdir(noContenoxDir)
	if err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	resolvedDir2, err := ResolveContenoxDir()
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	fallbackDir := filepath.Join(noContenoxDir, ".contenox")
	if resolvedDir2 != fallbackDir {
		t.Errorf("Expected fallback dir %q, got %q", fallbackDir, resolvedDir2)
	}
}
