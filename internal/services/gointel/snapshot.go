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

const loadMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedSyntax |
	packages.NeedTypes |
	packages.NeedTypesInfo |
	packages.NeedTypesSizes |
	packages.NeedImports |
	packages.NeedModule

// ToolchainView names the build context a result was produced under; treat it as a signal, not a verdict.
type ToolchainView struct {
	// GoVersion is the `go` binary that read the module graph.
	GoVersion string `json:"go_version"`
	// Checker is the Go release whose go/types produced the type information — this binary's own.
	Checker string `json:"checker"`
	GOOS    string `json:"goos"`
	GOARCH  string `json:"goarch"`
}

// String is the one-line form embedded in every result.
func (v ToolchainView) String() string {
	return fmt.Sprintf("%s %s/%s, type-checked by %s; tests excluded, no build tags; advisory — `go build` is the arbiter",
		v.GoVersion, v.GOOS, v.GOARCH, v.Checker)
}

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

// Snapshot is one module root, loaded and fully type-checked, immutable after build; queries read it without locking.
type Snapshot struct {
	// Root is the absolute module root.
	Root string
	// Base is the workspace root Root is contained in; every path in a result is rendered relative to it.
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

	pkgs        []*packages.Package
	byPath      map[string]*packages.Package
	byDir       map[string]*packages.Package
	byFile      map[string]*packages.Package
	importPaths map[string]struct{}

	files       map[string]stamp
	dirs        map[string]stamp
	moduleFiles map[string]stamp
	pkgFiles    map[string][]string
	pkgDir      map[string]string
}

func buildSnapshot(ctx context.Context, root, base string) (*Snapshot, error) {
	if err := lookPathGo(); err != nil {
		return nil, err
	}
	start := time.Now()
	cfg := &packages.Config{
		Context: ctx,
		Mode:    loadMode,
		Dir:     root,
		Tests:   false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		// go list stderr can be a page of text; echoErr clamps it.
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

const maxSourceFileBytes = 4 << 20

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

func (s *Snapshot) anchor(pos token.Pos) string {
	if !pos.IsValid() {
		return ""
	}
	p := s.Fset.Position(pos)
	return fmt.Sprintf("%s:%d:%d", displayPath(s.Base, p.Filename), p.Line, p.Column)
}
