package contenoxcli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnit_acpxIsReservedSubcommand(t *testing.T) {
	if !reservedSubcommands["acpx"] {
		t.Fatal(`"acpx" must be reserved so it is dispatched as a subcommand, not injected as run input`)
	}
	if !firstNonFlagIsReserved([]string{"acpx"}) {
		t.Fatal(`expected "acpx" to be recognized as a reserved subcommand`)
	}
}

func TestUnit_retiredServeIsReservedSubcommand(t *testing.T) {
	if !reservedSubcommands["serve"] {
		t.Fatal(`"serve" must stay reserved so the retired command is not injected as run input`)
	}
	if !firstNonFlagIsReserved([]string{"serve"}) {
		t.Fatal(`expected "serve" to be recognized as a retired reserved subcommand`)
	}
}

func TestUnit_promptInputIsNotReserved(t *testing.T) {
	if firstNonFlagIsReserved([]string{"summarise", "README.md"}) {
		t.Fatal("ordinary prompt input must remain eligible for default run injection")
	}
}

func TestUnit_seedHeadlessACPChainIfMissing(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, headlessACPChainFilename)

	if err := seedHeadlessACPChainIfMissing(dir); err != nil {
		t.Fatalf("seed when absent: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("expected %s written: %v", dst, err)
	}

	if err := os.WriteFile(dst, []byte("USER EDIT"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := seedHeadlessACPChainIfMissing(dir); err != nil {
		t.Fatalf("seed when present: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "USER EDIT" {
		t.Fatal("seedHeadlessACPChainIfMissing overwrote a user-edited chain file")
	}
}

func TestUnit_firstNonFlagIsReserved_version(t *testing.T) {
	if !firstNonFlagIsReserved([]string{"version"}) {
		t.Fatal(`expected "version" to be reserved so it is not passed to run/chat`)
	}
	if !firstNonFlagIsReserved([]string{"--db", "/tmp/x", "version"}) {
		t.Fatal(`expected first positional after flags to be recognized as version subcommand`)
	}
}

func TestUnit_resolveContenoxDir(t *testing.T) {
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
	// ResolveContenoxDir requires a workspace.id file to recognize a directory
	// as a valid workspace (so backups / pre-init dirs don't shadow the real one).
	if err := os.WriteFile(filepath.Join(contenoxDir, "workspace.id"), []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to write workspace.id: %v", err)
	}

	// 1. Test from sub2Dir. It should walk up and find it in projectDir.
	// t.Chdir restores the original cwd on cleanup — a plain os.Chdir would
	// leave the whole test process inside a deleted temp dir, breaking any
	// later test in the package that spawns a subprocess (getwd fails).
	t.Chdir(sub2Dir)

	resolvedDir, err := ResolveContenoxDir(nil)
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

	t.Chdir(noContenoxDir)

	resolvedDir2, err := ResolveContenoxDir(nil)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	fallbackDir := filepath.Join(noContenoxDir, ".contenox")
	if resolvedDir2 != fallbackDir {
		t.Errorf("Expected fallback dir %q, got %q", fallbackDir, resolvedDir2)
	}
}

// Bare invocations must route to session-backed chat, not the stateless run
// pipeline — this dispatch silently regressed to "run" once before (BUG-011),
// making the documented default-session auto-create a no-op.
func TestUnit_DispatchSubcommand_BarePromptIsChat(t *testing.T) {
	if got := dispatchSubcommand([]string{"say hello"}, false); got != "chat" {
		t.Fatalf("bare prompt dispatched to %q, want chat", got)
	}
	if got := dispatchSubcommand([]string{"--db", "x.db", "say hello"}, false); got != "chat" {
		t.Fatalf("bare prompt with flags dispatched to %q, want chat", got)
	}
	if got := dispatchSubcommand([]string{"--experimental-acp"}, false); got != "acp" {
		t.Fatalf("--experimental-acp dispatched to %q, want acp", got)
	}
	if got := dispatchSubcommand([]string{"run", "input"}, false); got != "" {
		t.Fatalf("explicit run subcommand re-dispatched to %q, want none", got)
	}
	if got := dispatchSubcommand([]string{"--help"}, true); got != "" {
		t.Fatalf("help-only dispatched to %q, want none", got)
	}
}
