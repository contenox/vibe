package contenoxcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/version"
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

func TestUnit_serveIsReservedSubcommand(t *testing.T) {
	if !reservedSubcommands["serve"] {
		t.Fatal(`"serve" must stay reserved so the host command is not injected as run input`)
	}
	if !firstNonFlagIsReserved([]string{"serve"}) {
		t.Fatal(`expected "serve" to be recognized as a reserved subcommand`)
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

// seeds chain-fim-default.json when absent, never overwrites a user edit.
func TestUnit_seedFIMChainIfMissing(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, SystemDirName, "chain-fim-default.json")

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
	if _, err := os.Stat(filepath.Join(dir, SystemDirName, "chain-fim-default.json")); !os.IsNotExist(err) {
		t.Fatalf("expected acpx to leave chain-fim-default.json unwritten, stat err: %v", err)
	}
}

// TestUnit_seedOptionalFIMChain_ACPSeeds proves acp, the editor profile, does
// seed the FIM chain preset via the same seedFIMChainIfMissing wired above.
func TestUnit_seedOptionalFIMChain_ACPSeeds(t *testing.T) {
	dir := t.TempDir()
	seedOptionalFIMChain(acpProfileACP, dir)
	if _, err := os.Stat(filepath.Join(dir, SystemDirName, "chain-fim-default.json")); err != nil {
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

// TestUnit_PrintVersion_NamesDirtyWorkingTreeBuilds pins I7's version half: a
// VCS-stamped build prints its revision, dirty flag, and build time, and a
// build with no stamp keeps the one-line release format byte-for-byte.
func TestUnit_PrintVersion_NamesDirtyWorkingTreeBuilds(t *testing.T) {
	var out strings.Builder
	printVersion(&out, "contenox", "v0.41.0", version.Provenance{})
	if got := out.String(); got != "contenox version v0.41.0\n" {
		t.Fatalf("unstamped build must keep the release format, got %q", got)
	}

	out.Reset()
	printVersion(&out, "contenox", "v0.41.0", version.Provenance{
		Revision: "41b11dd6", Dirty: true, Time: "2026-08-18T06:57:00Z",
	})
	got := out.String()
	if !strings.Contains(got, "contenox version v0.41.0\n") {
		t.Errorf("the version line must stay first, got %q", got)
	}
	if !strings.Contains(got, "revision 41b11dd6 (working tree modified)") {
		t.Errorf("a dirty working-tree build must say so, got %q", got)
	}
	if !strings.Contains(got, "built 2026-08-18T06:57:00Z") {
		t.Errorf("the build time must be printed, got %q", got)
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

	// 1. Test from sub2Dir.
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

// TestUnit_DispatchSubcommand_BareWordIsNotDispatched asserts a bare word never
// becomes a turn: it falls through to cobra, which reports an unknown command,
// so a mistyped subcommand costs nothing. A sentence is the opposite case and
// is pinned in the truth table above.
func TestUnit_DispatchSubcommand_BareWordIsNotDispatched(t *testing.T) {
	pinTerminal(t, true)
	pinPipedStdin(t, "")
	if got := dispatchSubcommand([]string{"sttaus"}, false); got != "" {
		t.Fatalf("bare word dispatched to %q, want no dispatch", got)
	}
	if got := dispatchSubcommand([]string{"--db", "x.db", "sttaus"}, false); got != "" {
		t.Fatalf("bare word with flags dispatched to %q, want no dispatch", got)
	}
}

func pinTerminal(t *testing.T, tty bool) {
	t.Helper()
	previous := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return tty }
	t.Cleanup(func() { stdoutIsTerminal = previous })
}

func pinPipedStdin(t *testing.T, body string) {
	t.Helper()
	previous := pipedStdin
	pipedStdin = func() (string, bool) { return body, body != "" }
	t.Cleanup(func() { pipedStdin = previous })
}

// TestUnit_DispatchSubcommand_TruthTable pins every front-door case at once,
// because they are only correct relative to each other: the same bare argument
// is a typo without a pipe and a task with one.
func TestUnit_DispatchSubcommand_TruthTable(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		tty      bool
		stdin    string
		onlyHelp bool
		want     string
	}{
		{"no args on a terminal opens beam", nil, true, "", true, "beam"},
		{"no args with nothing to draw on prints help", nil, false, "", true, ""},
		{"no args and no stdin still prints help", nil, false, "", true, ""},
		{"help flags ask for help even on a terminal", []string{"--help"}, true, "", true, ""},
		{"a single unreserved token is a mistyped subcommand", []string{"sttaus"}, true, "", false, ""},
		{"a single token stays a typo behind flags", []string{"--db", "x.db", "sttaus"}, true, "", false, ""},
		{"a sentence is a task", []string{"summarise what changed here"}, true, "", false, "run"},
		{"a sentence behind flags is a task", []string{"--db", "x.db", "summarise what changed here"}, true, "", false, "run"},
		{"a sentence with stdout redirected is still a task", []string{"summarise what changed here"}, false, "", false, "run"},
		{"a pipe makes one word real intent", []string{"review"}, false, "diff --git a/x b/x", false, "run"},
		{"a pipe carries a sentence too", []string{"review this and say what to check"}, false, "diff --git a/x b/x", false, "run"},
		{"a reserved word wins over the typo guard", []string{"doctor"}, true, "", false, ""},
		{"a reserved word wins over a pipe", []string{"doctor"}, false, "diff --git a/x b/x", false, ""},
		{"an explicit run is left alone", []string{"run", "summarise what changed here"}, true, "", false, ""},
		{"two positionals stay explicit", []string{"reviewer", "check the last commit"}, true, "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinTerminal(t, tc.tty)
			pinPipedStdin(t, tc.stdin)
			if got := dispatchSubcommand(tc.args, tc.onlyHelp); got != tc.want {
				t.Fatalf("dispatchSubcommand(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestUnit_IsSentence_IsTheWholeTypoGuard pins the one predicate standing
// between a fat-fingered subcommand and a paid model call.
func TestUnit_IsSentence_IsTheWholeTypoGuard(t *testing.T) {
	for _, single := range []string{"sttaus", "  sttaus  ", "mission-fire", "--", ""} {
		if isSentence(single) {
			t.Errorf("%q is one token and must not be read as a task", single)
		}
	}
	for _, sentence := range []string{"say hello", "a b", "review\nthis"} {
		if !isSentence(sentence) {
			t.Errorf("%q is a sentence and must be read as a task", sentence)
		}
	}
}

// TestUnit_NonFlagArgs_SkipsFlagsAndTheirValues pins the parser both dispatch
// halves share: firstNonFlagIsReserved and the implicit-run guard read the same
// positionals, so a flag that swallowed its value in one and not the other
// would make a reserved word reachable by one path only.
func TestUnit_NonFlagArgs_SkipsFlagsAndTheirValues(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"doctor"}, []string{"doctor"}},
		{[]string{"--db", "x.db", "doctor"}, []string{"doctor"}},
		{[]string{"--db=x.db", "doctor"}, []string{"doctor"}},
		{[]string{"--trace", "doctor"}, []string{"doctor"}},
		{[]string{"--", "doctor"}, []string{"doctor"}},
		{[]string{"--db", "x.db"}, nil},
		{[]string{"reviewer", "check this"}, []string{"reviewer", "check this"}},
	}
	for _, tc := range cases {
		got := nonFlagArgs(tc.args)
		if len(got) != len(tc.want) {
			t.Fatalf("nonFlagArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("nonFlagArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		}
	}
}

// TestUnit_DispatchSubcommand_BareInvocationOpensBeam pins the front door: with
// a terminal to draw on, `contenox` with no arguments is beam, not a help page.
// Without one there is nothing to draw on, so help stays the answer — and
// `contenox --help` asks for help on either.
func TestUnit_DispatchSubcommand_BareInvocationOpensBeam(t *testing.T) {
	pinTerminal(t, true)
	if got := dispatchSubcommand(nil, true); got != "beam" {
		t.Fatalf("bare invocation on a terminal dispatched to %q, want %q", got, "beam")
	}
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}} {
		if got := dispatchSubcommand(args, true); got != "" {
			t.Fatalf("%v dispatched to %q, want help", args, got)
		}
	}

	pinTerminal(t, false)
	if got := dispatchSubcommand(nil, true); got != "" {
		t.Fatalf("bare invocation with no terminal dispatched to %q, want help", got)
	}
}

// TestUnit_BeamCommandIsRegistered proves the name the bare dispatch injects is
// a real subcommand: dispatchSubcommand only prepends a string.
func TestUnit_BeamCommandIsRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Name() == "beam" {
			return
		}
	}
	t.Fatal("no `beam` command is registered, so a bare `contenox` would fail as an unknown command")
}
