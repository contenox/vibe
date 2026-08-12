package contenoxcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/libtracker"
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

func TestUnit_newIsReservedSubcommand(t *testing.T) {
	if !reservedSubcommands["new"] {
		t.Fatal(`"new" must be reserved so 'contenox new' opens the TUI instead of being injected as a chat prompt ("beam" alone covers only the retired name)`)
	}
	if !firstNonFlagIsReserved([]string{"new"}) {
		t.Fatal(`expected "new" to be recognized as a reserved subcommand`)
	}
}

func TestUnit_autocompleteIsReservedSubcommand(t *testing.T) {
	if !reservedSubcommands["autocomplete"] {
		t.Fatal(`"autocomplete" must be reserved so 'contenox autocomplete --stdio' dispatches to the stdio server instead of being injected as a chat prompt`)
	}
	if !firstNonFlagIsReserved([]string{"autocomplete", "--stdio"}) {
		t.Fatal(`expected "autocomplete" to be recognized as a reserved subcommand`)
	}
}

func TestUnit_shellCompletionRequestsAreReserved(t *testing.T) {
	for _, name := range []string{"__complete", "__completeNoDesc"} {
		if !reservedSubcommands[name] {
			t.Fatalf("%q must be reserved: the completion script invokes it on every TAB, and chat injection would run a model call per keystroke", name)
		}
	}
}

func TestUnit_promptInputIsNotReserved(t *testing.T) {
	if firstNonFlagIsReserved([]string{"summarise", "README.md"}) {
		t.Fatal("ordinary prompt input must remain eligible for default run injection")
	}
}

func TestUnit_seedHeadlessACPChainIfMissing(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, chainAgentACPXFilename)

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

// TestUnit_seedFIMChainIfMissing mirrors TestUnit_seedHeadlessACPChainIfMissing:
// seeds chain-fim-default.json when absent, never overwrites a user edit.
func TestUnit_seedFIMChainIfMissing(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "chain-fim-default.json")

	if err := seedFIMChainIfMissing(dir); err != nil {
		t.Fatalf("seed when absent: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("expected %s written: %v", dst, err)
	}

	if err := os.WriteFile(dst, []byte("USER EDIT"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := seedFIMChainIfMissing(dir); err != nil {
		t.Fatalf("seed when present: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "USER EDIT" {
		t.Fatal("seedFIMChainIfMissing overwrote a user-edited chain file")
	}
}

// TestUnit_seedOptionalFIMChain_ACPXDoesNotSeed proves acpx, which has no
// seedFIMChain wired (no editor buffer to complete into), never writes the
// FIM chain preset — the seed step must be a no-op for that profile.
func TestUnit_seedOptionalFIMChain_ACPXDoesNotSeed(t *testing.T) {
	dir := t.TempDir()
	seedOptionalFIMChain(acpProfileACPX, dir)
	if _, err := os.Stat(filepath.Join(dir, "chain-fim-default.json")); !os.IsNotExist(err) {
		t.Fatalf("expected acpx to leave chain-fim-default.json unwritten, stat err: %v", err)
	}
}

// TestUnit_seedOptionalFIMChain_ACPSeeds proves acp, the editor profile, does
// seed the FIM chain preset via the same seedFIMChainIfMissing wired above.
func TestUnit_seedOptionalFIMChain_ACPSeeds(t *testing.T) {
	dir := t.TempDir()
	seedOptionalFIMChain(acpProfileACP, dir)
	if _, err := os.Stat(filepath.Join(dir, "chain-fim-default.json")); err != nil {
		t.Fatalf("expected acp to seed chain-fim-default.json: %v", err)
	}
}

// TestUnit_loadOptionalFIMChain_PopulatesRegistryWhenPresent proves the FIM
// registry is populated for the acp profile when chain-fim-default.json (or
// its env override) resolves to a valid chain — the case that must produce a
// non-nil Deps.FIMChainRegistry so _contenox/autocomplete actually works on a
// real `contenox acp` run.
func TestUnit_loadOptionalFIMChain_PopulatesRegistryWhenPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom-fim-chain.json")
	chain := `{"id":"fim-test","tasks":[{"id":"only","handler":"chat_completion"}]}`
	if err := os.WriteFile(path, []byte(chain), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTENOX_ACP_FIM_CHAIN_PATH", path)

	tracker := libtracker.NoopTracker{}
	got := loadOptionalFIMChain(context.Background(), tracker, acpProfileACP)
	if got == nil || got.Default() == nil {
		t.Fatal("expected a populated FIM chain registry")
	}
	if got.Default().ID != "fim-test" {
		t.Fatalf("unexpected chain ID %q", got.Default().ID)
	}
}

// TestUnit_loadOptionalFIMChain_MissingFileDegradesToNil proves a missing or
// unparseable FIM chain does not error: autocomplete is optional, so startup
// must still succeed (chat keeps working) with a nil registry, which
// Transport.handleAutocomplete already turns into a clean method-not-found.
func TestUnit_loadOptionalFIMChain_MissingFileDegradesToNil(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONTENOX_ACP_FIM_CHAIN_PATH", filepath.Join(dir, "does-not-exist.json"))

	tracker := libtracker.NoopTracker{}
	got := loadOptionalFIMChain(context.Background(), tracker, acpProfileACP)
	if got != nil {
		t.Fatal("expected nil FIM chain registry for a missing chain file")
	}
}

// TestUnit_loadOptionalFIMChain_ACPXNeverLoads proves acpx never attempts to
// load a FIM chain at all (no seedFIMChain wired), regardless of what the env
// var points at — the profile gate, not just a load failure, is what keeps
// autocomplete off that profile.
func TestUnit_loadOptionalFIMChain_ACPXNeverLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom-fim-chain.json")
	chain := `{"id":"fim-test","tasks":[{"id":"only","handler":"chat_completion"}]}`
	if err := os.WriteFile(path, []byte(chain), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTENOX_ACP_FIM_CHAIN_PATH", path)

	tracker := libtracker.NoopTracker{}
	got := loadOptionalFIMChain(context.Background(), tracker, acpProfileACPX)
	if got != nil {
		t.Fatal("expected acpx to never load a FIM chain registry")
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

// TestUnit_DispatchSubcommand_BarePromptIsChat asserts bare invocations route to session-backed chat, not the stateless run pipeline.
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
