package gointel

import (
	"context"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/nilfunc"
	"golang.org/x/tools/go/analysis/passes/printf"
	"golang.org/x/tools/go/analysis/passes/shadow"
	"golang.org/x/tools/go/analysis/passes/unreachable"
	"golang.org/x/tools/go/analysis/passes/unusedresult"
	"golang.org/x/tools/go/packages"
)

// Diagnostic scopes.
const (
	// ScopeChanged reports on the packages whose files this index has observed change — via Invalidate or the mtime sweep — since the process started.
	ScopeChanged = "changed"
	// ScopePackage reports on one package, named by import path, package name, or a workspace-relative file.
	ScopePackage = "package"
	// ScopeAll reports on every package of the module.
	ScopeAll = "all"
)

var (
	defaultVetPasses = []*analysis.Analyzer{
		printf.Analyzer,
		unusedresult.Analyzer,
		unreachable.Analyzer,
		nilfunc.Analyzer,
	}
	optionalVetPasses = []*analysis.Analyzer{
		shadow.Analyzer,
	}
	allVetPasses = append(append([]*analysis.Analyzer{}, defaultVetPasses...), optionalVetPasses...)
)

// VetPasses returns the names of every curated analysis pass, defaults first.
func VetPasses() []string { return analyzerNames(allVetPasses) }

// DefaultVetPasses returns the names of the passes a diagnostics call runs when none are requested.
func DefaultVetPasses() []string { return analyzerNames(defaultVetPasses) }

func analyzerNames(list []*analysis.Analyzer) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Name)
	}
	return out
}

func resolvePasses(requested []string) ([]*analysis.Analyzer, error) {
	if len(requested) == 0 {
		return defaultVetPasses, nil
	}
	byName := map[string]*analysis.Analyzer{}
	for _, a := range allVetPasses {
		byName[a.Name] = a
	}
	// Every name is validated before "all" is honoured, so request order cannot change whether a typo is caught.
	var out []*analysis.Analyzer
	seen := map[string]struct{}{}
	wantsAll := false
	for _, raw := range requested {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if name == "all" {
			wantsAll = true
			continue
		}
		a, ok := byName[name]
		if !ok {
			return nil, recoverablef("gointel: unknown vet pass %s; available passes are %s (or \"all\")",
				echoArg(raw), strings.Join(VetPasses(), ", "))
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, a)
	}
	if wantsAll {
		return allVetPasses, nil
	}
	if len(out) == 0 {
		return defaultVetPasses, nil
	}
	return out, nil
}

// Diagnostic is one finding.
type Diagnostic struct {
	Location string `json:"location"`
	// Severity is "type-error" (the load's own parse/type errors) or "vet" (a curated analysis pass).
	Severity string `json:"severity"`
	// Category is the analyzer name, or "type" for a load error.
	Category string `json:"category"`
	Message  string `json:"message"`
	Line     string `json:"line,omitempty"`
}

// DiagnosticsResult is a scoped diagnostics sweep.
type DiagnosticsResult struct {
	Scope string `json:"scope"`
	// Passes names the analysis passes that actually ran, so a clean result cannot be mistaken for "everything was checked".
	Passes      []string     `json:"passes"`
	Packages    []string     `json:"packages"`
	TypeErrors  int          `json:"type_errors"`
	VetFindings int          `json:"vet_findings"`
	Total       int          `json:"total"`
	Shown       int          `json:"shown"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Note        string       `json:"note,omitempty"`
	// Toolchain names the build context these findings were produced under.
	Toolchain string `json:"toolchain"`
}

const (
	defaultDiagCap = 100
	maxDiagCap     = 500
	maxDiagPkgs    = 50
)

func (ix *index) Diagnostics(ctx context.Context, req Request) (*DiagnosticsResult, error) {
	limit := req.Max
	if limit <= 0 {
		limit = defaultDiagCap
	}
	if limit > maxDiagCap {
		limit = maxDiagCap
	}
	scope := strings.TrimSpace(strings.ToLower(req.Scope))
	if scope == "" {
		scope = ScopeChanged
	}
	switch scope {
	case ScopeChanged, ScopePackage, ScopeAll:
	default:
		return nil, recoverablef("gointel: unknown diagnostics scope %s; use %q, %q, or %q", echoArg(req.Scope), ScopeChanged, ScopePackage, ScopeAll)
	}

	passes, err := resolvePasses(req.Passes)
	if err != nil {
		return nil, err
	}

	e, snap, err := ix.entryAndSnapshot(ctx, req)
	if err != nil {
		return nil, err
	}

	targets, note, err := ix.diagnosticTargets(e, snap, scope, req)
	if err != nil {
		return nil, err
	}

	res := &DiagnosticsResult{
		Scope:       scope,
		Passes:      analyzerNames(passes),
		Packages:    []string{},
		Diagnostics: []Diagnostic{},
		Note:        note,
		Toolchain:   snap.Toolchain.String(),
	}
	var all []Diagnostic
	for _, p := range targets {
		res.Packages = append(res.Packages, p.PkgPath)
		typeErrs := snap.typeErrors(p)
		all = append(all, typeErrs...)
		res.TypeErrors += len(typeErrs)
		if len(typeErrs) > 0 {
			continue
		}
		vet := snap.vet(p, passes)
		all = append(all, vet...)
		res.VetFindings += len(vet)
	}
	if len(res.Packages) > maxDiagPkgs {
		res.Note = appendNote(res.Note, fmt.Sprintf("%d packages examined; listing the first %d", len(res.Packages), maxDiagPkgs))
		res.Packages = res.Packages[:maxDiagPkgs]
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Severity != all[j].Severity {
			// Type errors sort before vet findings.
			return all[i].Severity == "type-error"
		}
		return all[i].Location < all[j].Location
	})
	res.Total = len(all)
	if len(all) > limit {
		res.Note = appendNote(res.Note, fmt.Sprintf("+%d more not shown; raise max (ceiling %d) or narrow the scope", len(all)-limit, maxDiagCap))
		all = all[:limit]
	}
	if all != nil {
		res.Diagnostics = all
	}
	res.Shown = len(res.Diagnostics)
	if res.Total == 0 {
		res.Note = appendNote(res.Note, "clean")
	}
	res.Note = appendNote(res.Note, "advisory: `go build` is the arbiter; tests excluded")
	return res, nil
}

func (ix *index) diagnosticTargets(e *entry, snap *Snapshot, scope string, req Request) ([]*packages.Package, string, error) {
	switch scope {
	case ScopeAll:
		return snap.pkgs, "", nil

	case ScopePackage:
		target := strings.TrimSpace(req.Target)
		if target == "" {
			target = req.Dir
		}
		if target == "" {
			target = "."
		}
		if p := snap.packageForPath(target); p != nil {
			return []*packages.Package{p}, "", nil
		}
		matched := snap.matchPackages(target)
		if len(matched) == 0 {
			return nil, "", wrapRecoverable(ErrNotFound,
				"no package or file named %s in this module; pass an import path, a package name, or a workspace-relative .go file",
				echoArg(target))
		}
		return matched, "", nil

	default: // ScopeChanged
		changed := e.changedPaths()
		seen := map[string]struct{}{}
		var out []*packages.Package
		for _, path := range changed {
			p := snap.byFile[path]
			if p == nil {
				p = snap.byDir[path]
			}
			if p == nil {
				p = snap.byDir[filepath.Dir(path)]
			}
			if p == nil {
				continue
			}
			if _, dup := seen[p.PkgPath]; dup {
				continue
			}
			seen[p.PkgPath] = struct{}{}
			out = append(out, p)
		}
		if len(out) == 0 {
			return nil, "no Go file under this module has been observed to change since the index was built; call again with scope \"all\" to sweep the whole module, or \"package\" to name one", nil
		}
		return out, "", nil
	}
}

func (s *Snapshot) typeErrors(p *packages.Package) []Diagnostic {
	if len(p.Errors) == 0 {
		return nil
	}
	positioned := false
	for _, err := range p.Errors {
		if err.Kind != packages.ListError {
			positioned = true
			break
		}
	}
	out := make([]Diagnostic, 0, len(p.Errors))
	for _, err := range p.Errors {
		if positioned && err.Kind == packages.ListError {
			continue
		}
		loc, line := s.positionFromString(err.Pos)
		out = append(out, Diagnostic{
			Location: loc,
			Severity: "type-error",
			Category: "type",
			Message:  s.enrichTypeError(strings.TrimSpace(err.Msg)),
			Line:     line,
		})
	}
	return out
}

const importErrorPrefix = "could not import "

func (s *Snapshot) enrichTypeError(msg string) string {
	if !strings.HasPrefix(msg, importErrorPrefix) {
		return msg
	}
	rest := strings.TrimPrefix(msg, importErrorPrefix)
	path := rest
	if i := strings.IndexAny(rest, " \t("); i > 0 {
		path = rest[:i]
	}
	path = strings.Trim(path, `"`)
	if path == "" {
		return msg
	}
	if _, own := s.byPath[path]; own {
		return msg
	}
	return msg + " — " + path + " is a dependency, not a package of this module, so this is a module-resolution failure rather than a problem with the import line; check that go.mod requires it and that it has been downloaded (`go mod download`)"
}

func (s *Snapshot) positionFromString(pos string) (string, string) {
	if pos == "" || pos == "-" {
		return displayPath(s.Base, s.Root), ""
	}
	parts := strings.Split(pos, ":")
	if len(parts) < 2 {
		return pos, ""
	}
	file := strings.Join(parts[:len(parts)-2], ":")
	if len(parts) == 2 {
		file = parts[0]
	}
	rest := parts[len(parts)-2:]
	lineNo := 0
	fmt.Sscanf(rest[0], "%d", &lineNo)
	text := ""
	if lineNo > 0 {
		text = snippet(sourceLines(file), lineNo)
	}
	return displayPath(s.Base, file) + ":" + strings.Join(rest, ":"), text
}

func (s *Snapshot) vet(p *packages.Package, passes []*analysis.Analyzer) []Diagnostic {
	if p.Types == nil || p.TypesInfo == nil || len(p.Syntax) == 0 {
		return nil
	}
	r := &passRunner{
		pkg:      p,
		results:  map[*analysis.Analyzer]any{},
		failed:   map[*analysis.Analyzer]bool{},
		objFacts: map[objFactKey]analysis.Fact{},
		pkgFacts: map[pkgFactKey]analysis.Fact{},
		diags:    map[*analysis.Analyzer][]analysis.Diagnostic{},
	}
	var out []Diagnostic
	for _, a := range passes {
		if _, ok := r.run(a); !ok {
			continue
		}
		for _, d := range r.diags[a] {
			category := d.Category
			if category == "" {
				category = a.Name
			}
			pos := s.Fset.Position(d.Pos)
			out = append(out, Diagnostic{
				Location: s.anchor(d.Pos),
				Severity: "vet",
				Category: category,
				Message:  d.Message,
				Line:     snippet(sourceLines(pos.Filename), pos.Line),
			})
		}
	}
	return out
}

type objFactKey struct {
	obj  types.Object
	kind reflect.Type
}

type pkgFactKey struct {
	pkg  *types.Package
	kind reflect.Type
}

type passRunner struct {
	pkg      *packages.Package
	results  map[*analysis.Analyzer]any
	failed   map[*analysis.Analyzer]bool
	objFacts map[objFactKey]analysis.Fact
	pkgFacts map[pkgFactKey]analysis.Fact
	diags    map[*analysis.Analyzer][]analysis.Diagnostic
}

func (r *passRunner) run(a *analysis.Analyzer) (result any, ok bool) {
	if res, done := r.results[a]; done {
		return res, true
	}
	if r.failed[a] {
		return nil, false
	}
	inputs := make(map[*analysis.Analyzer]any, len(a.Requires))
	for _, req := range a.Requires {
		res, ok := r.run(req)
		if !ok {
			r.failed[a] = true
			return nil, false
		}
		inputs[req] = res
	}
	pass := &analysis.Pass{
		Analyzer:          a,
		Fset:              r.pkg.Fset,
		Files:             r.pkg.Syntax,
		OtherFiles:        r.pkg.OtherFiles,
		IgnoredFiles:      r.pkg.IgnoredFiles,
		Pkg:               r.pkg.Types,
		TypesInfo:         r.pkg.TypesInfo,
		TypesSizes:        r.pkg.TypesSizes,
		ResultOf:          inputs,
		Report:            func(d analysis.Diagnostic) { r.diags[a] = append(r.diags[a], d) },
		ReadFile:          os.ReadFile,
		ImportObjectFact:  r.importObjectFact,
		ExportObjectFact:  r.exportObjectFact,
		ImportPackageFact: r.importPackageFact,
		ExportPackageFact: r.exportPackageFact,
		AllObjectFacts:    r.allObjectFacts,
		AllPackageFacts:   r.allPackageFacts,
	}

	res, err := func() (res any, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("analyzer %s panicked: %v", a.Name, p)
			}
		}()
		return a.Run(pass)
	}()
	if err != nil {
		r.failed[a] = true
		return nil, false
	}
	r.results[a] = res
	return res, true
}

func copyFact(stored, ptr analysis.Fact) bool {
	src := reflect.ValueOf(stored)
	dst := reflect.ValueOf(ptr)
	if src.Kind() != reflect.Pointer || dst.Kind() != reflect.Pointer || src.Type() != dst.Type() {
		return false
	}
	dst.Elem().Set(src.Elem())
	return true
}

func (r *passRunner) importObjectFact(obj types.Object, ptr analysis.Fact) bool {
	if obj == nil || ptr == nil {
		return false
	}
	stored, ok := r.objFacts[objFactKey{obj: obj, kind: reflect.TypeOf(ptr)}]
	if !ok {
		return false
	}
	return copyFact(stored, ptr)
}

func (r *passRunner) exportObjectFact(obj types.Object, fact analysis.Fact) {
	if obj == nil || fact == nil {
		return
	}
	r.objFacts[objFactKey{obj: obj, kind: reflect.TypeOf(fact)}] = fact
}

func (r *passRunner) importPackageFact(pkg *types.Package, ptr analysis.Fact) bool {
	if pkg == nil || ptr == nil {
		return false
	}
	stored, ok := r.pkgFacts[pkgFactKey{pkg: pkg, kind: reflect.TypeOf(ptr)}]
	if !ok {
		return false
	}
	return copyFact(stored, ptr)
}

func (r *passRunner) exportPackageFact(fact analysis.Fact) {
	if fact == nil {
		return
	}
	r.pkgFacts[pkgFactKey{pkg: r.pkg.Types, kind: reflect.TypeOf(fact)}] = fact
}

func (r *passRunner) allObjectFacts() []analysis.ObjectFact {
	out := make([]analysis.ObjectFact, 0, len(r.objFacts))
	for key, fact := range r.objFacts {
		out = append(out, analysis.ObjectFact{Object: key.obj, Fact: fact})
	}
	return out
}

func (r *passRunner) allPackageFacts() []analysis.PackageFact {
	out := make([]analysis.PackageFact, 0, len(r.pkgFacts))
	for key, fact := range r.pkgFacts {
		out = append(out, analysis.PackageFact{Package: key.pkg, Fact: fact})
	}
	return out
}
