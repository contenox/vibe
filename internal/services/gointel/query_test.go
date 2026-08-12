package gointel

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUnit_Definition_KnownGroundTruth(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		symbol   string
		wantKind string
		wantLoc  string
		wantLine string
	}{
		{"package-qualified type", "shapes.Rect", "struct", "shapes/shapes.go:25:6", "type Rect struct {"},
		{"interface", "shapes.Shape", "interface", "shapes/shapes.go:8:6", "type Shape interface {"},
		{"const", "shapes.Unit", "const", "shapes/shapes.go:52:7", "const Unit = 1.0"},
		{"func", "shapes.Scale", "func", "shapes/shapes.go:58:6", "func Scale(r Rect, f float64) Rect {"},
		{"method via pkg.Type.Method", "shapes.Rect.Area", "method", "shapes/shapes.go:33:15", "func (r Rect) Area() float64 { return r.W * r.H }"},
		{"field via pkg.Type.Field", "shapes.Rect.W", "field", "shapes/shapes.go:27:2", "W float64"},
		{"bare type name", "Circle", "struct", "shapes/shapes.go:39:6", "type Circle struct {"},
		{"bare Type.Method", "Circle.Name", "method", "shapes/shapes.go:48:17", "func (c Circle) Name() string { return \"circle\" }"},
		{"full import path", "example.com/fixture/report.Total", "func", "report/report.go:20:6", "func Total(list []shapes.Shape) float64 {"},
		{"import-path suffix", "fixture/report.Describe", "func", "report/report.go:29:6", "func Describe(s shapes.Shape) string {"},
		{"unexported", "shapes.notShape", "struct", "shapes/shapes.go:64:6", "type notShape struct{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ix.Definition(ctx, Request{Symbol: tc.symbol})
			if err != nil {
				t.Fatalf("definition(%q): %v", tc.symbol, err)
			}
			if res.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", res.Kind, tc.wantKind)
			}
			if res.Location != tc.wantLoc {
				t.Errorf("location = %q, want %q", res.Location, tc.wantLoc)
			}
			if res.Line != tc.wantLine {
				t.Errorf("line = %q, want %q", res.Line, tc.wantLine)
			}
		})
	}
}

func TestUnit_Describe_StructCarriesFieldsAndMethods(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	res, err := ix.Describe(context.Background(), Request{Symbol: "shapes.Rect"})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if res.Symbol != "example.com/fixture/shapes.Rect" {
		t.Errorf("symbol = %q", res.Symbol)
	}
	if res.Kind != "struct" {
		t.Errorf("kind = %q, want struct", res.Kind)
	}
	if res.Underlying != "struct{W float64; H float64}" {
		t.Errorf("underlying = %q", res.Underlying)
	}
	if !strings.HasPrefix(res.Doc, "Rect is an axis-aligned rectangle.") {
		t.Errorf("doc = %q, want the declaration's doc comment", res.Doc)
	}
	if !strings.Contains(res.Doc, "multi-line doc") {
		t.Errorf("doc = %q, want the second paragraph too", res.Doc)
	}
	if len(res.Fields) != 2 {
		t.Fatalf("%d fields, want 2", len(res.Fields))
	}
	if res.Fields[0].Name != "W" || res.Fields[0].Type != "float64" || res.Fields[0].Doc != "W is the width." {
		t.Errorf("field[0] = %+v", res.Fields[0])
	}
	if len(res.Methods) != 2 {
		t.Fatalf("%d methods, want 2 (Area, Name)", len(res.Methods))
	}
	names := []string{res.Methods[0].Name, res.Methods[1].Name}
	if names[0] != "Area" || names[1] != "Name" {
		t.Errorf("methods = %v, want [Area Name]", names)
	}
	if res.Methods[0].Type != "func() float64" {
		t.Errorf("Area type = %q", res.Methods[0].Type)
	}
}

func TestUnit_Describe_InterfaceListsItsMethodSet(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	res, err := ix.Describe(context.Background(), Request{Symbol: "shapes.Shape"})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if res.Kind != "interface" {
		t.Fatalf("kind = %q, want interface", res.Kind)
	}
	if len(res.Methods) != 2 {
		t.Fatalf("%d methods, want 2", len(res.Methods))
	}
	if res.Methods[0].Doc != "Area reports the shape's area." {
		t.Errorf("method doc = %q", res.Methods[0].Doc)
	}
	if len(res.Fields) != 0 {
		t.Errorf("an interface reported %d fields", len(res.Fields))
	}
}

func TestUnit_Describe_MethodCarriesTheSourceSignature(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	res, err := ix.Describe(context.Background(), Request{Symbol: "shapes.Rect.Area"})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if res.Signature != "func (r Rect) Area() float64" {
		t.Errorf("signature = %q", res.Signature)
	}
	if res.Kind != "method" {
		t.Errorf("kind = %q, want method", res.Kind)
	}
	if res.Doc != "Area implements Shape for Rect." {
		t.Errorf("doc = %q", res.Doc)
	}
}

func TestUnit_References_KnownGroundTruth(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))
	ctx := context.Background()

	t.Run("const used in both packages", func(t *testing.T) {
		res, err := ix.References(ctx, Request{Symbol: "shapes.Unit"})
		if err != nil {
			t.Fatalf("references: %v", err)
		}
		if res.Total != 3 || res.Uses != 5 || res.Shown != 3 {
			t.Fatalf("total=%d uses=%d shown=%d, want 3/5/3", res.Total, res.Uses, res.Shown)
		}
		if len(res.Files) != 2 {
			t.Fatalf("%d files, want 2", len(res.Files))
		}
		if res.Files[0].File != "report/report.go" || res.Files[1].File != "shapes/shapes.go" {
			t.Fatalf("files = %q, %q — want sorted, workspace-relative", res.Files[0].File, res.Files[1].File)
		}
		if res.Files[0].Lines[0].Line != 17 || res.Files[0].Lines[0].Uses != 2 {
			t.Errorf("report line 0 = %+v, want line 17 with 2 uses collapsed", res.Files[0].Lines[0])
		}
		if !strings.Contains(res.Files[0].Lines[0].Text, "var Default = shapes.Rect{") {
			t.Errorf("snippet = %q", res.Files[0].Lines[0].Text)
		}
		if res.Definition != "shapes/shapes.go:52:7" {
			t.Errorf("definition = %q", res.Definition)
		}
	})

	t.Run("single cross-package call site", func(t *testing.T) {
		res, err := ix.References(ctx, Request{Symbol: "shapes.Scale"})
		if err != nil {
			t.Fatalf("references: %v", err)
		}
		if res.Total != 1 || res.Uses != 1 {
			t.Fatalf("total=%d uses=%d, want 1/1", res.Total, res.Uses)
		}
		if res.Files[0].File != "report/report.go" || res.Files[0].Lines[0].Line != 45 {
			t.Fatalf("location = %s:%d, want report/report.go:45", res.Files[0].File, res.Files[0].Lines[0].Line)
		}
	})

	t.Run("type used from declarations and literals", func(t *testing.T) {
		res, err := ix.References(ctx, Request{Symbol: "shapes.Rect"})
		if err != nil {
			t.Fatalf("references: %v", err)
		}
		if res.Total != 7 || res.Uses != 8 {
			t.Fatalf("total=%d uses=%d, want 7/8", res.Total, res.Uses)
		}
	})

	t.Run("a same-named symbol in another package is not counted", func(t *testing.T) {
		reportUnit, err := ix.References(ctx, Request{Symbol: "report.Unit"})
		if err != nil {
			t.Fatalf("references: %v", err)
		}
		if reportUnit.Uses != 1 {
			t.Fatalf("report.Unit uses = %d, want 1 (a text search would find 5)", reportUnit.Uses)
		}
		if reportUnit.Files[0].File != "report/report.go" {
			t.Fatalf("report.Unit found in %q", reportUnit.Files[0].File)
		}
	})
}

func TestUnit_References_CapAndNote(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	res, err := ix.References(context.Background(), Request{Symbol: "shapes.Rect", Max: 2})
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if res.Shown != 2 {
		t.Fatalf("shown = %d, want 2", res.Shown)
	}
	if res.Total != 7 {
		t.Fatalf("total = %d, want the full count even when capped", res.Total)
	}
	if !strings.Contains(res.Note, "+5 more locations not shown") {
		t.Fatalf("note = %q, want a '+N more' notice", res.Note)
	}
	if !strings.Contains(res.Note, "ceiling 200") {
		t.Fatalf("note = %q, want the ceiling named", res.Note)
	}
}

func TestUnit_References_CapIsClampedToTheCeiling(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	res, err := ix.References(context.Background(), Request{Symbol: "shapes.Rect", Max: 1_000_000})
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if res.Shown != 7 {
		t.Fatalf("shown = %d, want all 7", res.Shown)
	}
}

func TestUnit_Implementations_BothDirections(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))
	ctx := context.Background()

	t.Run("interface to implementers", func(t *testing.T) {
		res, err := ix.Implementations(ctx, Request{Symbol: "shapes.Shape"})
		if err != nil {
			t.Fatalf("implementations: %v", err)
		}
		got := implNames(res.Implementers)
		want := []string{"example.com/fixture/shapes.Circle", "example.com/fixture/shapes.Rect"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("implementers = %v, want %v", got, want)
		}
		for _, e := range res.Implementers {
			if e.Receiver != "value" {
				t.Errorf("%s receiver = %q, want value", e.Name, e.Receiver)
			}
		}
		for _, name := range got {
			if strings.HasSuffix(name, ".notShape") {
				t.Error("notShape was reported as a Shape implementer")
			}
		}
		if names := implNames(res.Interfaces); strings.Join(names, ",") != "example.com/fixture/shapes.Named" {
			t.Fatalf("interfaces = %v, want [shapes.Named]", names)
		}
	})

	t.Run("concrete type to interfaces", func(t *testing.T) {
		res, err := ix.Implementations(ctx, Request{Symbol: "shapes.Rect"})
		if err != nil {
			t.Fatalf("implementations: %v", err)
		}
		got := implNames(res.Interfaces)
		want := []string{"example.com/fixture/shapes.Named", "example.com/fixture/shapes.Shape"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("interfaces = %v, want %v", got, want)
		}
		if len(res.Implementers) != 0 {
			t.Errorf("a struct reported %d implementers", len(res.Implementers))
		}
	})

	t.Run("near miss implements nothing", func(t *testing.T) {
		res, err := ix.Implementations(ctx, Request{Symbol: "shapes.notShape"})
		if err != nil {
			t.Fatalf("implementations: %v", err)
		}
		if len(res.Interfaces) != 0 {
			t.Fatalf("notShape reported %v", implNames(res.Interfaces))
		}
	})

	t.Run("non-type is refused with a teaching error", func(t *testing.T) {
		_, err := ix.Implementations(ctx, Request{Symbol: "shapes.Scale"})
		if err == nil || !strings.Contains(err.Error(), "not a type") {
			t.Fatalf("error = %v, want a not-a-type refusal", err)
		}
	})
}

func implNames(entries []ImplEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func TestUnit_Symbols_PackageOutline(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	res, err := ix.Symbols(context.Background(), Request{Target: "shapes"})
	if err != nil {
		t.Fatalf("symbols: %v", err)
	}
	if res.Target != "example.com/fixture/shapes" || res.Kind != "package" {
		t.Fatalf("target=%q kind=%q", res.Target, res.Kind)
	}
	if res.Total != 13 || res.Shown != 13 {
		t.Fatalf("total=%d shown=%d, want 13/13", res.Total, res.Shown)
	}
	// Exported first, then by name.
	if res.Symbols[0].Name != "Circle" {
		t.Fatalf("first symbol = %q, want Circle", res.Symbols[0].Name)
	}
	last := res.Symbols[len(res.Symbols)-1]
	if last.Name != "notShape.Area" {
		t.Fatalf("last symbol = %q, want the unexported ones sorted to the end", last.Name)
	}
	byName := map[string]Symbol{}
	for _, s := range res.Symbols {
		byName[s.Name] = s
	}
	for name, wantKind := range map[string]string{
		"Shape":     "interface",
		"Rect":      "struct",
		"Rect.Area": "method",
		"Scale":     "func",
		"Unit":      "const",
		"UnitRect":  "var",
	} {
		got, ok := byName[name]
		if !ok {
			t.Errorf("%s missing from the outline", name)
			continue
		}
		if got.Kind != wantKind {
			t.Errorf("%s kind = %q, want %q", name, got.Kind, wantKind)
		}
		if !strings.HasPrefix(got.Location, "shapes/shapes.go:") {
			t.Errorf("%s location = %q", name, got.Location)
		}
	}
}

func TestUnit_Symbols_FileOutline(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	res, err := ix.Symbols(context.Background(), Request{Target: "report/report.go"})
	if err != nil {
		t.Fatalf("symbols: %v", err)
	}
	if res.Kind != "file" || res.Target != "report/report.go" {
		t.Fatalf("kind=%q target=%q", res.Kind, res.Target)
	}
	if res.Total != 6 {
		t.Fatalf("total = %d, want 6", res.Total)
	}
}

func TestUnit_Symbols_DefaultsToThePackageAtDir(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	res, err := ix.Symbols(context.Background(), Request{Dir: "report"})
	if err != nil {
		t.Fatalf("symbols: %v", err)
	}
	if res.Target != "example.com/fixture/report" {
		t.Fatalf("target = %q", res.Target)
	}
}

func TestUnit_Symbols_CapAndUnknownTarget(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))
	ctx := context.Background()

	res, err := ix.Symbols(ctx, Request{Target: "shapes", Max: 3})
	if err != nil {
		t.Fatalf("symbols: %v", err)
	}
	if res.Shown != 3 || res.Total != 13 {
		t.Fatalf("shown=%d total=%d", res.Shown, res.Total)
	}
	if !strings.Contains(res.Note, "+10 more symbols") {
		t.Fatalf("note = %q", res.Note)
	}

	if _, err := ix.Symbols(ctx, Request{Target: "nosuchpkg"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestUnit_Resolve_AmbiguousNameListsQualifiedCandidates(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))

	_, err := ix.Definition(context.Background(), Request{Symbol: "Unit"})
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("error = %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{
		"example.com/fixture/report.Unit",
		"example.com/fixture/shapes.Unit",
		"re-ask with one of these qualified names",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestUnit_Resolve_NotFoundVariants(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))
	ctx := context.Background()

	t.Run("absent name", func(t *testing.T) {
		_, err := ix.Definition(ctx, Request{Symbol: "Nope"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(err.Error(), "go_symbols") {
			t.Errorf("error %q does not name the next step", err)
		}
	})

	t.Run("symbol in a dependency", func(t *testing.T) {
		_, err := ix.Definition(ctx, Request{Symbol: "strings.Builder"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(err.Error(), "dependency") || !strings.Contains(err.Error(), "example.com/fixture") {
			t.Errorf("error %q does not explain the index boundary", err)
		}
	})

	t.Run("did-you-mean over the module", func(t *testing.T) {
		_, err := ix.Definition(ctx, Request{Symbol: "report.Scale"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(err.Error(), "example.com/fixture/shapes.Scale") {
			t.Errorf("error %q carries no suggestion", err)
		}
	})

	t.Run("unknown package qualifier", func(t *testing.T) {
		_, err := ix.Definition(ctx, Request{Symbol: "nosuch.Thing"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestUnit_Resolve_MalformedSymbols(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))
	ctx := context.Background()

	for _, tc := range []struct{ symbol, want string }{
		{"", "symbol is required"},
		{"a.b.c.d", "too many dotted parts"},
		{"shapes..Rect", "empty name part"},
		{"(*Rect).Area", "not a symbol name"},
		{"example.com/fixture/shapes", "names a package, not a symbol"},
	} {
		_, err := ix.Definition(ctx, Request{Symbol: tc.symbol})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("definition(%q) = %v, want an error containing %q", tc.symbol, err, tc.want)
		}
	}
}

func TestUnit_Resolve_EverySurfaceCarriesTheRecoverableMarker(t *testing.T) {
	ix := newTestIndex(t, newFixture(t, "fixture"))
	ctx := context.Background()

	_, e1 := ix.Definition(ctx, Request{Symbol: "Nope"})
	_, e2 := ix.Describe(ctx, Request{Symbol: "Unit"})
	_, e3 := ix.Symbols(ctx, Request{Target: "nosuch"})
	_, e4 := ix.Diagnostics(ctx, Request{Scope: "sideways"})
	for i, err := range []error{e1, e2, e3, e4} {
		if err == nil {
			t.Fatalf("case %d unexpectedly succeeded", i)
		}
		if !strings.Contains(err.Error(), severityRecoverable) {
			t.Errorf("case %d error %q lacks the recoverable severity marker", i, err)
		}
	}
}
