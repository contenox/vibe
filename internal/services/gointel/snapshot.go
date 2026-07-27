package gointel

import (
	"context"
	"fmt"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

// loadMode is the go/packages mode every snapshot is built with.
//
// The blueprint's build order names NeedName|NeedFiles|NeedSyntax|NeedTypes|
// NeedTypesInfo|NeedDeps. This is that set MINUS NeedDeps and PLUS NeedImports
// and NeedModule, and the deviation is measured, not stylistic:
//
//	mode                       load (this repo, 101 pkgs)   retained heap
//	with NeedDeps                                    4.4s        1299 MB
//	without NeedDeps                        0.85s (warm)         113 MB
//
// NeedDeps combined with NeedSyntax makes go/packages type-check every
// DEPENDENCY from source too — all of the standard library and every vendored
// tree — instead of reading their export data. It buys nothing V1 asks for and
// costs 11x the memory. The blueprint's own risk section budgets "hundreds of
// MB warm" and bounds the cache at two roots; 1.3 GB per snapshot blows that
// budget outright, and the reaper cannot save a process that has already
// allocated it.
//
// Nothing is lost. Every package OF THIS MODULE is a root of the "./..." load,
// so every one of them carries Syntax, Types and TypesInfo. Cross-package
// types.Object identity — the thing go_references and go_implementations are
// built on — is a property of the loader sharing one *types.Package per import
// path across the graph, not of NeedDeps. Verified against the blueprint's own
// spike measurement: both modes return the same 18 references to
// frame.StyleBrand across the same 8 files.
//
// The three additions are cheap and load-bearing: NeedImports keeps the import
// graph visible without NeedDeps (used to tell "this symbol lives in a
// dependency" from "this symbol does not exist"), NeedModule names the module in
// results, and NeedTypesSizes populates Pass.TypesSizes for the go/analysis
// driver in diags.go.
const loadMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedSyntax |
	packages.NeedTypes |
	packages.NeedTypesInfo |
	packages.NeedTypesSizes |
	packages.NeedImports |
	packages.NeedModule

// ToolchainView names the build context a result was produced under. It rides
// on every tool result because of the blueprint's advisory rule: the go/types
// compiled into this binary is not necessarily the one the repo builds with, so
// a result is a strong signal and never a verdict.
type ToolchainView struct {
	// GoVersion is the `go` binary that read the module graph.
	GoVersion string `json:"go_version"`
	// Checker is the Go release whose go/types produced the type information —
	// this binary's own. A gap between the two is where phantom errors come from.
	Checker string `json:"checker"`
	GOOS    string `json:"goos"`
	GOARCH  string `json:"goarch"`
}

// String is the one-line form embedded in every result.
func (v ToolchainView) String() string {
	return fmt.Sprintf("%s %s/%s, type-checked by %s; tests excluded, no build tags; advisory — `go build` is the arbiter",
		v.GoVersion, v.GOOS, v.GOARCH, v.Checker)
}

// stamp is a file's identity for the mtime sweep. Size rides along with the
// modification time because some filesystems keep coarse timestamps, and a
// same-second rewrite that changes length must not read as unchanged.
type stamp struct {
	modNs int64
	size  int64
	ok    bool
}

func stampOf(path string) stamp {
	info, err := os.Stat(path)
	if err != nil {
		return stamp{}
	}
	return stamp{modNs: info.ModTime().UnixNano(), size: info.Size(), ok: true}
}

// Snapshot is one module root, loaded and fully type-checked, IMMUTABLE after
// build. Queries read it without locking; freshness is the cache's problem
// (loader.go), never the snapshot's.
type Snapshot struct {
	// Root is the absolute module root.
	Root string
	// Base is the workspace root Root is contained in. Every path in a result is
	// rendered relative to it.
	Base string
	// ModulePath is the module path from go.mod, when the driver reported one.
	ModulePath string
	// Fset positions every object in this snapshot.
	Fset *token.FileSet
	// Toolchain is the build context this snapshot was produced under.
	Toolchain ToolchainView
	// BuiltAt and BuildDuration are the load's own telemetry.
	BuiltAt       time.Time
	BuildDuration time.Duration

	// pkgs are the module's own packages (the roots of "./..."), the only
	// packages with syntax and type info.
	pkgs   []*packages.Package
	byPath map[string]*packages.Package
	byDir  map[string]*packages.Package
	byFile map[string]*packages.Package
	// importPaths is every import path any module package pulls in, used to tell
	// "declared in a dependency gointel does not index" from "does not exist".
	importPaths map[string]struct{}

	// files/dirs/moduleFiles are the mtime-sweep record.
	files       map[string]stamp
	dirs        map[string]stamp
	moduleFiles map[string]stamp
	pkgFiles    map[string][]string
	pkgDir      map[string]string
}

// buildSnapshot loads root and freezes the result.
func buildSnapshot(ctx context.Context, root, base string) (*Snapshot, error) {
	if err := lookPathGo(); err != nil {
		return nil, err
	}
	start := time.Now()
	cfg := &packages.Config{
		Context: ctx,
		Mode:    loadMode,
		Dir:     root,
		// Tests excluded by default (blueprint decision, restated in every tool
		// description). Loading tests doubles the package count and makes every
		// "who calls this" answer noisier than it is useful.
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		// The driver error is clamped: go/packages forwards `go list` stderr
		// verbatim, which on a broken module is a page of text the model can do
		// nothing with beyond the first line.
		return nil, wrapRecoverable(ErrLoad,
			"could not load %s: %s; run `go build ./...` there to see what the toolchain is complaining about",
			displayPath(base, root), echoErr(err))
	}
	if len(pkgs) == 0 {
		return nil, wrapRecoverable(ErrNoModule,
			"%s contains a go.mod but no Go packages; gointel has nothing to index there",
			displayPath(base, root))
	}

	s := &Snapshot{
		Root:        root,
		Base:        base,
		Fset:        pkgs[0].Fset,
		Toolchain:   toolchainView(ctx, root),
		BuiltAt:     time.Now(),
		pkgs:        pkgs,
		byPath:      make(map[string]*packages.Package, len(pkgs)),
		byDir:       make(map[string]*packages.Package, len(pkgs)),
		byFile:      map[string]*packages.Package{},
		importPaths: map[string]struct{}{},
		files:       map[string]stamp{},
		dirs:        map[string]stamp{},
		moduleFiles: map[string]stamp{},
		pkgFiles:    map[string][]string{},
		pkgDir:      map[string]string{},
	}

	for _, p := range pkgs {
		s.byPath[p.PkgPath] = p
		if p.Module != nil && p.Module.Path != "" && s.ModulePath == "" && p.Module.Dir == root {
			s.ModulePath = p.Module.Path
		}
		for imp := range p.Imports {
			s.importPaths[imp] = struct{}{}
		}
		s.pkgFiles[p.PkgPath] = append([]string(nil), p.GoFiles...)
		for _, f := range p.GoFiles {
			s.byFile[f] = p
			s.files[f] = stampOf(f)
		}
		if len(p.GoFiles) > 0 {
			dir := filepath.Dir(p.GoFiles[0])
			s.byDir[dir] = p
			s.pkgDir[p.PkgPath] = dir
			s.dirs[dir] = stampOf(dir)
		}
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		path := filepath.Join(root, name)
		if st := stampOf(path); st.ok {
			s.moduleFiles[path] = st
		}
	}
	s.BuildDuration = time.Since(start)
	return s, nil
}

// toolchainView asks the go binary that will drive the load which toolchain it
// is. One extra process per snapshot build, next to the `go list` invocations
// go/packages already makes — and the alternative (assuming this binary's own
// version) is exactly the lie the advisory rule exists to prevent.
func toolchainView(ctx context.Context, root string) ToolchainView {
	v := ToolchainView{
		GoVersion: "unknown",
		Checker:   runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
	cmd := exec.CommandContext(ctx, "go", "env", "GOVERSION", "GOOS", "GOARCH")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return v
	}
	fields := strings.Split(strings.ReplaceAll(strings.TrimSpace(string(out)), "\r\n", "\n"), "\n")
	if len(fields) >= 3 {
		if f := strings.TrimSpace(fields[0]); f != "" {
			v.GoVersion = f
		}
		if f := strings.TrimSpace(fields[1]); f != "" {
			v.GOOS = f
		}
		if f := strings.TrimSpace(fields[2]); f != "" {
			v.GOARCH = f
		}
	}
	return v
}

// sweepSet returns the file and directory stamps to verify for a query that
// consulted pkgPaths. A nil pkgPaths returns everything.
func (s *Snapshot) sweepSet(pkgPaths []string) (map[string]stamp, map[string]stamp) {
	if pkgPaths == nil {
		return s.files, s.dirs
	}
	files := make(map[string]stamp, 16)
	dirs := make(map[string]stamp, len(pkgPaths))
	for _, path := range pkgPaths {
		for _, f := range s.pkgFiles[path] {
			if st, ok := s.files[f]; ok {
				files[f] = st
			}
		}
		if dir, ok := s.pkgDir[path]; ok {
			if st, ok := s.dirs[dir]; ok {
				dirs[dir] = st
			}
		}
	}
	return files, dirs
}

// moduleLabel names the indexed module the way an error message should: by
// module path when the driver reported one, otherwise by workspace-relative
// root.
func (s *Snapshot) moduleLabel() string {
	if s.ModulePath != "" {
		return "the module " + s.ModulePath
	}
	return "the module rooted at " + displayPath(s.Base, s.Root)
}

// Packages returns the module's own package import paths, sorted.
func (s *Snapshot) Packages() []string {
	out := make([]string, 0, len(s.byPath))
	for path := range s.byPath {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// matchPackages resolves a package qualifier to the module packages it names.
//
// Most specific wins, and only the winning tier is returned, so "frame" does not
// come back ambiguous just because two unrelated trees both end in a directory
// called frame when one of them matched exactly.
//
//  1. exact import path            github.com/x/y/frame
//  2. import-path suffix           surfaces/beamtui/frame
//  3. package name                 frame
func (s *Snapshot) matchPackages(qual string) []*packages.Package {
	qual = strings.TrimSpace(qual)
	if qual == "" {
		return nil
	}
	if p, ok := s.byPath[qual]; ok {
		return []*packages.Package{p}
	}
	var suffix, named []*packages.Package
	for _, p := range s.pkgs {
		if strings.HasSuffix(p.PkgPath, "/"+qual) {
			suffix = append(suffix, p)
			continue
		}
		if p.Name == qual {
			named = append(named, p)
		}
	}
	if len(suffix) > 0 {
		sortPackages(suffix)
		return suffix
	}
	sortPackages(named)
	return named
}

func sortPackages(list []*packages.Package) {
	sort.Slice(list, func(i, j int) bool { return list[i].PkgPath < list[j].PkgPath })
}

// packageForPath resolves a file or directory path (workspace-relative or
// absolute) to the module package that owns it.
func (s *Snapshot) packageForPath(path string) *packages.Package {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.Base, filepath.FromSlash(path))
	}
	abs = filepath.Clean(abs)
	if p, ok := s.byFile[abs]; ok {
		return p
	}
	if p, ok := s.byDir[abs]; ok {
		return p
	}
	return nil
}

// maxSourceFileBytes bounds the files this package will read to quote a line.
// A generated file of tens of megabytes should cost a missing snippet, never a
// stall.
const maxSourceFileBytes = 4 << 20

// sourceLines reads a file as lines for snippet quoting. Errors are not
// propagated: a missing snippet degrades a result, it does not invalidate it.
func sourceLines(path string) []string {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSourceFileBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

// maxSnippetRunes bounds a quoted source line. Long generated lines must not be
// what dominates a references result.
const maxSnippetRunes = 160

func snippet(lines []string, line int) string {
	if line < 1 || line > len(lines) {
		return ""
	}
	text := strings.TrimSpace(lines[line-1])
	r := []rune(text)
	if len(r) > maxSnippetRunes {
		return string(r[:maxSnippetRunes]) + "…"
	}
	return text
}

// anchor renders a position the way the read tools address files:
// workspace-relative path, then line, then column.
func (s *Snapshot) anchor(pos token.Pos) string {
	if !pos.IsValid() {
		return ""
	}
	p := s.Fset.Position(pos)
	return fmt.Sprintf("%s:%d:%d", displayPath(s.Base, p.Filename), p.Line, p.Column)
}
