package gointel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnit_Diagnostics_VetPassSetIsTheCuratedOne(t *testing.T) {
	if got, want := strings.Join(VetPasses(), ","), "printf,unusedresult,unreachable,nilfunc,shadow"; got != want {
		t.Fatalf("VetPasses() = %s, want %s", got, want)
	}
	// shadow is deliberately not in the default set.
	if got, want := strings.Join(DefaultVetPasses(), ","), "printf,unusedresult,unreachable,nilfunc"; got != want {
		t.Fatalf("DefaultVetPasses() = %s, want %s", got, want)
	}
}

func TestUnit_Diagnostics_ShadowIsOptIn(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "badfixture"))
	ctx := context.Background()

	quiet, err := ix.Diagnostics(ctx, Request{Scope: ScopePackage, Target: "shadowpkg"})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if quiet.Total != 0 {
		t.Fatalf("the default pass set reported %d findings on shadowpkg: %+v", quiet.Total, quiet.Diagnostics)
	}
	if strings.Join(quiet.Passes, ",") != strings.Join(DefaultVetPasses(), ",") {
		t.Fatalf("passes = %v, want the default set named on the result", quiet.Passes)
	}

	loud, err := ix.Diagnostics(ctx, Request{Scope: ScopePackage, Target: "shadowpkg", Passes: []string{"shadow"}})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if loud.VetFindings != 1 {
		t.Fatalf("shadow findings = %d, want 1: %+v", loud.VetFindings, loud.Diagnostics)
	}
	if loud.Diagnostics[0].Category != "shadow" {
		t.Fatalf("category = %q", loud.Diagnostics[0].Category)
	}
	if strings.Join(loud.Passes, ",") != "shadow" {
		t.Fatalf("passes = %v, want only the requested one", loud.Passes)
	}

	all, err := ix.Diagnostics(ctx, Request{Scope: ScopePackage, Target: "shadowpkg", Passes: []string{"all"}})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if all.VetFindings != 1 || strings.Join(all.Passes, ",") != strings.Join(VetPasses(), ",") {
		t.Fatalf("passes=%v findings=%d", all.Passes, all.VetFindings)
	}

	if _, err := ix.Diagnostics(ctx, Request{Scope: ScopeAll, Passes: []string{"fieldalignment"}}); err == nil ||
		!strings.Contains(err.Error(), "unknown vet pass") {
		t.Fatalf("error = %v, want an unknown-pass refusal", err)
	}
}

func TestUnit_Diagnostics_ScopeAllReportsTypeErrorsAndVetFindings(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "badfixture"))

	res, err := ix.Diagnostics(context.Background(), Request{Scope: ScopeAll})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if res.TypeErrors != 1 {
		t.Fatalf("type errors = %d, want 1 (the go list stderr duplicate must be dropped)", res.TypeErrors)
	}
	if res.VetFindings != 2 {
		t.Fatalf("vet findings = %d, want 2", res.VetFindings)
	}
	if res.Total != 3 || res.Shown != 3 {
		t.Fatalf("total=%d shown=%d", res.Total, res.Shown)
	}

	// Type errors sort ahead of advisory vet findings.
	if res.Diagnostics[0].Severity != "type-error" {
		t.Fatalf("first diagnostic severity = %q", res.Diagnostics[0].Severity)
	}
	first := res.Diagnostics[0]
	if first.Location != "typeerr/typeerr.go:9:14" {
		t.Errorf("location = %q", first.Location)
	}
	if !strings.Contains(first.Message, "cannot use \"not an int\"") {
		t.Errorf("message = %q", first.Message)
	}
	if first.Line != `var n int = "not an int"` {
		t.Errorf("quoted line = %q", first.Line)
	}

	byCategory := map[string]Diagnostic{}
	for _, d := range res.Diagnostics {
		byCategory[d.Category] = d
	}
	printf, ok := byCategory["printf"]
	if !ok {
		t.Fatal("no printf finding")
	}
	if printf.Location != "vetpkg/vet.go:11:22" || !strings.Contains(printf.Message, "reads arg #1, but call has 0 args") {
		t.Errorf("printf finding = %+v", printf)
	}
	if printf.Severity != "vet" {
		t.Errorf("printf severity = %q, want vet", printf.Severity)
	}
	unreachable, ok := byCategory["unreachable"]
	if !ok {
		t.Fatal("no unreachable finding")
	}
	if unreachable.Location != "vetpkg/vet.go:18:2" {
		t.Errorf("unreachable finding = %+v", unreachable)
	}
}

func TestUnit_Diagnostics_AnalyzersSkipPackagesThatDoNotTypeCheck(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "badfixture"))

	res, err := ix.Diagnostics(context.Background(), Request{Scope: ScopePackage, Target: "typeerr"})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if res.TypeErrors != 1 {
		t.Fatalf("type errors = %d, want 1", res.TypeErrors)
	}
	if res.VetFindings != 0 {
		t.Fatalf("vet findings = %d — analyzers must not run on a package that does not type-check", res.VetFindings)
	}
}

func TestUnit_Diagnostics_ScopePackageAcceptsAFilePath(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "badfixture"))

	res, err := ix.Diagnostics(context.Background(), Request{Scope: ScopePackage, Target: "vetpkg/vet.go"})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if len(res.Packages) != 1 || res.Packages[0] != "example.com/badfixture/vetpkg" {
		t.Fatalf("packages = %v", res.Packages)
	}
	if res.VetFindings != 2 || res.TypeErrors != 0 {
		t.Fatalf("vet=%d typeErrors=%d", res.VetFindings, res.TypeErrors)
	}
}

func TestUnit_Diagnostics_CleanPackageSaysSo(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	res, err := ix.Diagnostics(context.Background(), Request{Scope: ScopeAll})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if res.Total != 0 {
		t.Fatalf("clean fixture reported %d diagnostics: %+v", res.Total, res.Diagnostics)
	}
	if !strings.Contains(res.Note, "clean") {
		t.Fatalf("note = %q, want it to say clean", res.Note)
	}
	if res.Diagnostics == nil {
		t.Fatal("Diagnostics is nil rather than an empty list")
	}
}

func TestUnit_Diagnostics_ScopeChangedFollowsObservedEdits(t *testing.T) {
	root := newFixture(t, "badfixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()

	// Nothing has been observed to change yet: an empty result that names the
	// next call, rather than a silent zero.
	res, err := ix.Diagnostics(ctx, Request{Scope: ScopeChanged})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if res.Total != 0 || len(res.Packages) != 0 {
		t.Fatalf("scope=changed reported %d diagnostics before any edit", res.Total)
	}
	if !strings.Contains(res.Note, "scope \"all\"") {
		t.Fatalf("note = %q, want a pointer to the next call", res.Note)
	}

	vet := filepath.Join(root, "vetpkg", "vet.go")
	ix.Invalidate(vet)

	res, err = ix.Diagnostics(ctx, Request{Scope: ScopeChanged})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if len(res.Packages) != 1 || res.Packages[0] != "example.com/badfixture/vetpkg" {
		t.Fatalf("packages = %v, want only the changed one", res.Packages)
	}
	if res.VetFindings != 2 {
		t.Fatalf("vet findings = %d, want 2", res.VetFindings)
	}
	if res.TypeErrors != 0 {
		t.Fatalf("the untouched typeerr package leaked into scope=changed")
	}
}

func TestUnit_Diagnostics_ScopeChangedSeesAFreshEdit(t *testing.T) {
	root := newFixture(t, "badfixture")
	ix := newTestIndex(t, root)
	ctx := context.Background()

	if _, err := ix.Diagnostics(ctx, Request{Scope: ScopePackage, Target: "vetpkg"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	// Fix the printf mistake and announce it; the finding must be gone.
	vet := filepath.Join(root, "vetpkg", "vet.go")
	fixed := "package vetpkg\n\nimport \"fmt\"\n\n// PrintfMistake is now correct.\nfunc PrintfMistake() string {\n\treturn fmt.Sprintf(\"%d\", 1)\n}\n\n// Unreachable still has dead code.\nfunc Unreachable() int {\n\treturn 1\n\tfmt.Println(\"dead\")\n\treturn 2\n}\n"
	writeFixtureFile(t, vet, fixed)
	ix.Invalidate(vet)

	res, err := ix.Diagnostics(ctx, Request{Scope: ScopeChanged})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	for _, d := range res.Diagnostics {
		if d.Category == "printf" {
			t.Fatalf("printf finding survived the fix: %+v", d)
		}
	}
	if res.VetFindings != 1 {
		t.Fatalf("vet findings = %d, want 1 (only the unreachable one)", res.VetFindings)
	}
}

func TestUnit_Diagnostics_CapAndUnknownScope(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "badfixture"))
	ctx := context.Background()

	res, err := ix.Diagnostics(ctx, Request{Scope: ScopeAll, Max: 1})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if res.Shown != 1 || res.Total != 3 {
		t.Fatalf("shown=%d total=%d", res.Shown, res.Total)
	}
	if !strings.Contains(res.Note, "+2 more") {
		t.Fatalf("note = %q", res.Note)
	}

	if _, err := ix.Diagnostics(ctx, Request{Scope: "sideways"}); err == nil ||
		!strings.Contains(err.Error(), "unknown diagnostics scope") {
		t.Fatalf("error = %v", err)
	}
	if _, err := ix.Diagnostics(ctx, Request{Scope: ScopePackage, Target: "nosuch"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestUnit_Diagnostics_AlwaysNamesTheToolchainView(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "badfixture"))
	ctx := context.Background()

	for _, scope := range []string{ScopeAll, ScopeChanged, ""} {
		res, err := ix.Diagnostics(ctx, Request{Scope: scope})
		if err != nil {
			t.Fatalf("scope %q: %v", scope, err)
		}
		if res.Toolchain == "" {
			t.Fatalf("scope %q produced a result with no toolchain view", scope)
		}
		for _, want := range []string{"tests excluded", "advisory", "go build"} {
			if !strings.Contains(res.Toolchain, want) {
				t.Errorf("scope %q toolchain %q missing %q", scope, res.Toolchain, want)
			}
		}
		if !strings.Contains(res.Note, "advisory: `go build` is the arbiter") {
			t.Errorf("scope %q note %q does not restate the advisory rule", scope, res.Note)
		}
	}
}

func TestUnit_Diagnostics_LoadErrorWhenTheModuleIsBroken(t *testing.T) {
	root := newFixture(t, "fixture")
	// A go.mod the go tool cannot parse: the driver fails rather than returning
	// packages, and the error must teach rather than leak a driver dump.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("this is not a go.mod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := newTestIndex(t, root)

	_, err := ix.Diagnostics(context.Background(), Request{Scope: ScopeAll})
	if err == nil {
		t.Fatal("a broken go.mod was loaded without complaint")
	}
	if !strings.Contains(err.Error(), "gointel:") {
		t.Fatalf("error %q is not in the package's voice", err)
	}
}
