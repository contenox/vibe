package gointel

import (
	"context"
	"fmt"
	"go/ast"
	"go/printer"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
)

// Names-first resolution. Every query here starts from one of:
//
//	Ident                bare, resolved across every package of the module
//	pkg.Ident            package-qualified (name, path suffix, or full path)
//	Type.Member          bare type, field or method
//	pkg.Type.Member      fully qualified member
//
// Ambiguity is refused, never guessed: a name matching two declarations comes back as an error listing both qualified names, so the next call is exact.

// symbolRef is a resolved symbol: the object plus the package it was found in and, for a member, the named type that owns it.
type symbolRef struct {
	pkg  *packages.Package
	obj  types.Object
	recv string
}

func (r symbolRef) qualified() string {
	if r.pkg == nil || r.obj == nil {
		return ""
	}
	if r.recv != "" {
		return r.pkg.PkgPath + "." + r.recv + "." + r.obj.Name()
	}
	return r.pkg.PkgPath + "." + r.obj.Name()
}

// splitSymbol separates an optional package qualifier from the dotted name parts. A slash anywhere means everything up to the last one is an import-path prefix, so "a/b/c.T.M" is package "a/b/c", type T, member M.
func splitSymbol(name string) (pkgQual string, parts []string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, recoverablef("gointel: symbol is required, e.g. \"pkg.Ident\", \"pkg.Type.Method\", or a bare \"Ident\"")
	}
	if strings.ContainsAny(name, " \t\n\r(){}[]*&") {
		return "", nil, recoverablef("gointel: %s is not a symbol name; pass a plain identifier such as \"pkg.Ident\" or \"pkg.Type.Method\" (no parentheses, pointers, or spaces)", echoArg(name))
	}
	rest := name
	prefix := ""
	if i := strings.LastIndex(name, "/"); i >= 0 {
		prefix = name[:i+1]
		rest = name[i+1:]
	}
	parts = strings.Split(rest, ".")
	for _, p := range parts {
		if p == "" {
			return "", nil, recoverablef("gointel: %s has an empty name part; write \"pkg.Ident\" or \"pkg.Type.Method\"", echoArg(name))
		}
	}
	if prefix != "" {
		pkgQual = prefix + parts[0]
		parts = parts[1:]
	}
	return pkgQual, parts, nil
}

// candidates gathers every declaration a name could mean, without deciding.
func (s *Snapshot) candidates(name string) ([]symbolRef, error) {
	pkgQual, parts, err := splitSymbol(name)
	if err != nil {
		return nil, err
	}
	var out []symbolRef
	switch {
	case pkgQual != "":
		matched := s.matchPackages(pkgQual)
		switch len(parts) {
		case 0:
			return nil, recoverablef("gointel: %s names a package, not a symbol; use go_symbols to outline a package", echoArg(name))
		case 1:
			for _, p := range matched {
				out = append(out, scopeRefs(p, parts[0])...)
			}
		case 2:
			for _, p := range matched {
				out = append(out, memberRefs(p, parts[0], parts[1])...)
			}
		default:
			return nil, tooManyParts(name)
		}
	case len(parts) == 1:
		for _, p := range s.pkgs {
			out = append(out, scopeRefs(p, parts[0])...)
		}
	case len(parts) == 2:
		// "a.b" is genuinely two shapes — package.Ident and Type.Member — and both are searched; if both hit, that is one reported ambiguity.
		for _, p := range s.matchPackages(parts[0]) {
			out = append(out, scopeRefs(p, parts[1])...)
		}
		for _, p := range s.pkgs {
			out = append(out, memberRefs(p, parts[0], parts[1])...)
		}
	case len(parts) == 3:
		for _, p := range s.matchPackages(parts[0]) {
			out = append(out, memberRefs(p, parts[1], parts[2])...)
		}
	default:
		return nil, tooManyParts(name)
	}
	return dedupeRefs(out), nil
}

func tooManyParts(name string) error {
	return recoverablef("gointel: %s has too many dotted parts; the deepest form gointel resolves is \"pkg.Type.Member\"", echoArg(name))
}

// resolve narrows candidates to exactly one, or explains why it cannot.
func (s *Snapshot) resolve(name string) (symbolRef, error) {
	cands, err := s.candidates(name)
	if err != nil {
		return symbolRef{}, err
	}
	switch len(cands) {
	case 1:
		return cands[0], nil
	case 0:
		return symbolRef{}, s.notFound(name)
	}
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.qualified())
	}
	sort.Strings(names)
	shown := names
	if len(shown) > 8 {
		shown = append(append([]string(nil), shown[:8]...), fmt.Sprintf("+%d more", len(names)-8))
	}
	return symbolRef{}, wrapRecoverable(ErrAmbiguous,
		"%s matches %d declarations: %s; re-ask with one of these qualified names",
		echoArg(name), len(cands), strings.Join(shown, ", "))
}

func scopeRefs(p *packages.Package, ident string) []symbolRef {
	if p.Types == nil {
		return nil
	}
	obj := p.Types.Scope().Lookup(ident)
	if obj == nil {
		return nil
	}
	return []symbolRef{{pkg: p, obj: obj}}
}

func memberRefs(p *packages.Package, typeName, member string) []symbolRef {
	if p.Types == nil {
		return nil
	}
	tn, ok := p.Types.Scope().Lookup(typeName).(*types.TypeName)
	if !ok {
		return nil
	}
	// addressable=true so pointer-receiver methods resolve from the value type — an agent writes "Rect.Scale", not "(*Rect).Scale".
	obj, _, _ := types.LookupFieldOrMethod(tn.Type(), true, p.Types, member)
	if obj == nil {
		return nil
	}
	return []symbolRef{{pkg: p, obj: obj, recv: typeName}}
}

func dedupeRefs(in []symbolRef) []symbolRef {
	if len(in) < 2 {
		return in
	}
	seen := make(map[types.Object]struct{}, len(in))
	out := in[:0]
	for _, r := range in {
		if _, dup := seen[r.obj]; dup {
			continue
		}
		seen[r.obj] = struct{}{}
		out = append(out, r)
	}
	return out
}

// notFound builds the teaching error for a name that resolved to nothing: the name lives in a dependency, something close exists (suggest, never act), or the name is simply absent.
func (s *Snapshot) notFound(name string) error {
	pkgQual, parts, err := splitSymbol(name)
	if err != nil {
		return err
	}
	qual := pkgQual
	if qual == "" && len(parts) > 1 {
		qual = parts[0]
	}
	if qual != "" && len(s.matchPackages(qual)) == 0 {
		if dep := s.dependencyFor(qual); dep != "" {
			return wrapRecoverable(ErrNotFound,
				"%s is not declared in this module; %s resolves to the dependency %s, and gointel indexes only %s",
				echoArg(name), echoArg(qual), dep, s.moduleLabel())
		}
		return wrapRecoverable(ErrNotFound,
			"no package named %s in this module; go_symbols with a package path lists what a package declares",
			echoArg(qual))
	}
	ident := parts[len(parts)-1]
	if hints := s.suggest(ident, 5); len(hints) > 0 {
		return wrapRecoverable(ErrNotFound,
			"%s is not declared in this module. Did you mean: %s?",
			echoArg(name), strings.Join(hints, ", "))
	}
	return wrapRecoverable(ErrNotFound,
		"%s is not declared in this module (%d packages indexed); go_symbols outlines a package if you are looking for the right name",
		echoArg(name), len(s.pkgs))
}

// dependencyFor reports the import path a qualifier names among this module's dependencies, or "".
func (s *Snapshot) dependencyFor(qual string) string {
	if _, ok := s.importPaths[qual]; ok {
		return qual
	}
	best := ""
	for path := range s.importPaths {
		if strings.HasSuffix(path, "/"+qual) || path == qual {
			if best == "" || len(path) < len(best) {
				best = path
			}
		}
	}
	return best
}

// suggest returns up to limit qualified names resembling ident; the caller never resolves to a suggestion.
func (s *Snapshot) suggest(ident string, limit int) []string {
	if ident == "" {
		return nil
	}
	lower := strings.ToLower(ident)
	var exact, fold []string
	for _, p := range s.pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, n := range scope.Names() {
			switch {
			case n == ident:
				exact = append(exact, p.PkgPath+"."+n)
			case strings.ToLower(n) == lower:
				fold = append(fold, p.PkgPath+"."+n)
			}
		}
	}
	sort.Strings(exact)
	sort.Strings(fold)
	out := append(exact, fold...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Result payloads: every payload is structured, hard-capped, and anchored with workspace-relative file:line. No payload ever carries a whole file; that is what the read tools are for.

// DefinitionResult is where a symbol is declared.
type DefinitionResult struct {
	Symbol    string `json:"symbol"`
	Kind      string `json:"kind"`
	Location  string `json:"location"`
	Line      string `json:"line,omitempty"`
	Module    string `json:"module,omitempty"`
	Toolchain string `json:"toolchain"`
}

// Member is one field or method of a named type.
type Member struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Type     string `json:"type"`
	Doc      string `json:"doc,omitempty"`
	Location string `json:"location,omitempty"`
}

// DescribeResult is hover-grade truth about a symbol.
type DescribeResult struct {
	Symbol     string   `json:"symbol"`
	Kind       string   `json:"kind"`
	Type       string   `json:"type,omitempty"`
	Signature  string   `json:"signature,omitempty"`
	Doc        string   `json:"doc,omitempty"`
	Location   string   `json:"location"`
	Underlying string   `json:"underlying,omitempty"`
	Fields     []Member `json:"fields,omitempty"`
	Methods    []Member `json:"methods,omitempty"`
	Note       string   `json:"note,omitempty"`
	Toolchain  string   `json:"toolchain"`
}

// RefLine is one location that uses a symbol. Uses is set only when the same line mentions the symbol more than once.
type RefLine struct {
	Line int    `json:"line"`
	Text string `json:"text,omitempty"`
	Uses int    `json:"uses,omitempty"`
}

// RefFile groups locations by the file they occur in. Count is the number of distinct lines in that file.
type RefFile struct {
	File  string    `json:"file"`
	Count int       `json:"count"`
	Lines []RefLine `json:"lines"`
}

// ReferencesResult is every use of a symbol in this module. Total counts distinct file:line locations (what the caps apply to); Uses counts raw identifier occurrences.
type ReferencesResult struct {
	Symbol     string    `json:"symbol"`
	Definition string    `json:"definition"`
	Total      int       `json:"total"`
	Uses       int       `json:"uses"`
	Shown      int       `json:"shown"`
	Files      []RefFile `json:"files"`
	Note       string    `json:"note,omitempty"`
	Toolchain  string    `json:"toolchain"`
}

// ImplEntry is one end of an implements relation.
type ImplEntry struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Receiver string `json:"receiver,omitempty"`
	Location string `json:"location"`
}

// ImplementationsResult answers in both directions.
type ImplementationsResult struct {
	Symbol string `json:"symbol"`
	Kind   string `json:"kind"`
	// Implementers is populated when Symbol is an interface.
	Implementers []ImplEntry `json:"implementers,omitempty"`
	// Interfaces is populated when Symbol is a concrete type: the module interfaces it satisfies.
	Interfaces []ImplEntry `json:"interfaces,omitempty"`
	Note       string      `json:"note,omitempty"`
	Toolchain  string      `json:"toolchain"`
}

// Symbol is one entry in an outline.
type Symbol struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Type     string `json:"type,omitempty"`
	Location string `json:"location"`
}

// SymbolsResult is a package or file outline.
type SymbolsResult struct {
	Target    string   `json:"target"`
	Kind      string   `json:"kind"`
	Total     int      `json:"total"`
	Shown     int      `json:"shown"`
	Symbols   []Symbol `json:"symbols"`
	Note      string   `json:"note,omitempty"`
	Toolchain string   `json:"toolchain"`
}

func (ix *index) Definition(ctx context.Context, req Request) (*DefinitionResult, error) {
	var out *DefinitionResult
	err := ix.withSnapshot(ctx, req, func(s *Snapshot) ([]string, error) {
		ref, err := s.resolve(req.Symbol)
		if err != nil {
			return nil, err
		}
		pos := s.Fset.Position(ref.obj.Pos())
		out = &DefinitionResult{
			Symbol:    ref.qualified(),
			Kind:      kindOf(ref.obj),
			Location:  s.anchor(ref.obj.Pos()),
			Line:      snippet(sourceLines(pos.Filename), pos.Line),
			Module:    s.ModulePath,
			Toolchain: s.Toolchain.String(),
		}
		return []string{ref.pkg.PkgPath}, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// maxMembers caps the fields and the methods a describe result lists. A struct with 300 fields is a real thing; 300 fields in one tool result is not useful.
const maxMembers = 60

// maxDocRunes caps a doc comment; the first paragraphs are what carries the contract.
const maxDocRunes = 1200

// maxMemberDocRunes caps a per-member doc to its first line.
const maxMemberDocRunes = 120

func (ix *index) Describe(ctx context.Context, req Request) (*DescribeResult, error) {
	var out *DescribeResult
	err := ix.withSnapshot(ctx, req, func(s *Snapshot) ([]string, error) {
		ref, err := s.resolve(req.Symbol)
		if err != nil {
			return nil, err
		}
		out = s.describe(ref)
		return []string{ref.pkg.PkgPath}, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Snapshot) describe(ref symbolRef) *DescribeResult {
	qual := types.RelativeTo(ref.pkg.Types)
	res := &DescribeResult{
		Symbol:    ref.qualified(),
		Kind:      kindOf(ref.obj),
		Type:      types.TypeString(ref.obj.Type(), qual),
		Doc:       clampRunes(s.docFor(ref), maxDocRunes),
		Location:  s.anchor(ref.obj.Pos()),
		Toolchain: s.Toolchain.String(),
	}
	if _, isFunc := ref.obj.(*types.Func); isFunc {
		res.Signature = s.signatureSource(ref)
		if res.Signature == "" {
			res.Signature = types.ObjectString(ref.obj, qual)
		}
	}

	tn, isType := ref.obj.(*types.TypeName)
	if !isType {
		return res
	}
	named, ok := types.Unalias(tn.Type()).(*types.Named)
	if !ok {
		res.Underlying = types.TypeString(tn.Type().Underlying(), qual)
		return res
	}
	res.Underlying = types.TypeString(named.Underlying(), qual)

	switch u := named.Underlying().(type) {
	case *types.Struct:
		for i := 0; i < u.NumFields() && len(res.Fields) < maxMembers; i++ {
			f := u.Field(i)
			res.Fields = append(res.Fields, Member{
				Name:     f.Name(),
				Kind:     "field",
				Type:     types.TypeString(f.Type(), qual),
				Doc:      clampRunes(firstLine(s.docFor(symbolRef{pkg: ref.pkg, obj: f})), maxMemberDocRunes),
				Location: s.anchor(f.Pos()),
			})
		}
		if u.NumFields() > len(res.Fields) {
			res.Note = appendNote(res.Note, fmt.Sprintf("+%d more fields", u.NumFields()-len(res.Fields)))
		}
	case *types.Interface:
		for i := 0; i < u.NumMethods() && len(res.Methods) < maxMembers; i++ {
			m := u.Method(i)
			res.Methods = append(res.Methods, Member{
				Name:     m.Name(),
				Kind:     "method",
				Type:     types.TypeString(m.Type(), qual),
				Doc:      clampRunes(firstLine(s.docFor(symbolRef{pkg: ref.pkg, obj: m})), maxMemberDocRunes),
				Location: s.anchor(m.Pos()),
			})
		}
		if u.NumMethods() > len(res.Methods) {
			res.Note = appendNote(res.Note, fmt.Sprintf("+%d more methods", u.NumMethods()-len(res.Methods)))
		}
		return res
	}

	// The pointer method set carries value-receiver and pointer-receiver methods plus everything promoted from embedded types. typeutil.MethodSetCache is not used: it is not safe for concurrent use, and a Snapshot is read from several goroutines at once.
	mset := types.NewMethodSet(types.NewPointer(named))
	total := mset.Len()
	for i := 0; i < total && len(res.Methods) < maxMembers; i++ {
		sel := mset.At(i)
		fn, ok := sel.Obj().(*types.Func)
		if !ok {
			continue
		}
		res.Methods = append(res.Methods, Member{
			Name:     fn.Name(),
			Kind:     "method",
			Type:     types.TypeString(fn.Type(), qual),
			Doc:      clampRunes(firstLine(s.docFor(symbolRef{pkg: ref.pkg, obj: fn})), maxMemberDocRunes),
			Location: s.anchor(fn.Pos()),
		})
	}
	if total > len(res.Methods) {
		res.Note = appendNote(res.Note, fmt.Sprintf("+%d more methods", total-len(res.Methods)))
	}
	sortMembers(res.Methods)
	return res
}

// defaultRefCap and maxRefCap bound a references result; the ceiling exists so a per-call override cannot turn one tool result into the whole context window.
const (
	defaultRefCap = 50
	maxRefCap     = 200
)

func (ix *index) References(ctx context.Context, req Request) (*ReferencesResult, error) {
	limit := req.Max
	if limit <= 0 {
		limit = defaultRefCap
	}
	if limit > maxRefCap {
		limit = maxRefCap
	}
	var out *ReferencesResult
	err := ix.withSnapshot(ctx, req, func(s *Snapshot) ([]string, error) {
		ref, err := s.resolve(req.Symbol)
		if err != nil {
			return nil, err
		}
		out = s.references(ref, limit)
		// nil: a references answer depends on every package in the module.
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Snapshot) references(ref symbolRef, limit int) *ReferencesResult {
	// file -> line -> occurrences on that line.
	byFile := map[string]map[int]int{}
	uses := 0
	for _, p := range s.pkgs {
		if p.TypesInfo == nil {
			continue
		}
		for id, obj := range p.TypesInfo.Uses {
			if obj != ref.obj {
				continue
			}
			pos := s.Fset.Position(id.Pos())
			lines, ok := byFile[pos.Filename]
			if !ok {
				lines = map[int]int{}
				byFile[pos.Filename] = lines
			}
			lines[pos.Line]++
			uses++
		}
	}

	files := make([]string, 0, len(byFile))
	total := 0
	for f, lines := range byFile {
		files = append(files, f)
		total += len(lines)
	}
	sort.Strings(files)

	res := &ReferencesResult{
		Symbol:     ref.qualified(),
		Definition: s.anchor(ref.obj.Pos()),
		Total:      total,
		Uses:       uses,
		Files:      []RefFile{},
		Toolchain:  s.Toolchain.String(),
	}
	shown := 0
	for _, f := range files {
		lineNos := make([]int, 0, len(byFile[f]))
		for ln := range byFile[f] {
			lineNos = append(lineNos, ln)
		}
		sort.Ints(lineNos)
		entry := RefFile{File: displayPath(s.Base, f), Count: len(lineNos)}
		src := sourceLines(f)
		for _, ln := range lineNos {
			if shown >= limit {
				break
			}
			line := RefLine{Line: ln, Text: snippet(src, ln)}
			if n := byFile[f][ln]; n > 1 {
				line.Uses = n
			}
			entry.Lines = append(entry.Lines, line)
			shown++
		}
		res.Files = append(res.Files, entry)
		if shown >= limit {
			break
		}
	}
	res.Shown = shown
	if total > shown {
		res.Note = appendNote(res.Note, fmt.Sprintf("+%d more locations not shown; raise max (ceiling %d) or narrow the symbol", total-shown, maxRefCap))
	}
	if fn, ok := ref.obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil && types.IsInterface(sig.Recv().Type()) {
			res.Note = appendNote(res.Note, "this is an interface method: these are calls THROUGH the interface, not uses of any implementation — use go_implementations to reach those")
		}
	}
	res.Note = appendNote(res.Note, "scope: this module's packages, tests excluded")
	return res
}

func (ix *index) Implementations(ctx context.Context, req Request) (*ImplementationsResult, error) {
	var out *ImplementationsResult
	err := ix.withSnapshot(ctx, req, func(s *Snapshot) ([]string, error) {
		ref, err := s.resolve(req.Symbol)
		if err != nil {
			return nil, err
		}
		out, err = s.implementations(ref)
		if err != nil {
			return nil, err
		}
		// Every named type in the module took part in the answer.
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type namedType struct {
	pkg   *packages.Package
	obj   *types.TypeName
	named *types.Named
}

// namedTypes enumerates every package-scope named type in the module, sorted so results are deterministic. Function-local types are out of scope: they cannot be named in a query.
func (s *Snapshot) namedTypes() []namedType {
	var out []namedType
	for _, p := range s.pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := types.Unalias(tn.Type()).(*types.Named)
			if !ok {
				continue
			}
			out = append(out, namedType{pkg: p, obj: tn, named: named})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].pkg.PkgPath != out[j].pkg.PkgPath {
			return out[i].pkg.PkgPath < out[j].pkg.PkgPath
		}
		return out[i].obj.Name() < out[j].obj.Name()
	})
	return out
}

func (s *Snapshot) implementations(ref symbolRef) (*ImplementationsResult, error) {
	tn, ok := ref.obj.(*types.TypeName)
	if !ok {
		return nil, recoverablef("gointel: %s is a %s, not a type; go_implementations relates types to interfaces", ref.qualified(), kindOf(ref.obj))
	}
	target, ok := types.Unalias(tn.Type()).(*types.Named)
	if !ok {
		return nil, recoverablef("gointel: %s is not a named type; go_implementations relates named types to interfaces", ref.qualified())
	}
	res := &ImplementationsResult{
		Symbol:    ref.qualified(),
		Kind:      kindOf(ref.obj),
		Toolchain: s.Toolchain.String(),
	}
	all := s.namedTypes()

	if iface, isIface := target.Underlying().(*types.Interface); isIface {
		if iface.NumMethods() == 0 {
			res.Note = "the empty interface is satisfied by every type; nothing to list"
			return res, nil
		}
		for _, nt := range all {
			if nt.named == target {
				continue
			}
			switch {
			case types.Implements(nt.named, iface):
				recv := "value"
				if types.IsInterface(nt.named) {
					recv = ""
				}
				res.Implementers = append(res.Implementers, s.implEntry(nt, recv))
			case types.Implements(types.NewPointer(nt.named), iface):
				res.Implementers = append(res.Implementers, s.implEntry(nt, "pointer"))
			}
		}
		if len(res.Implementers) == 0 {
			res.Note = appendNote(res.Note, "no type in this module implements it")
		}
	}

	// Both directions: an interface can itself satisfy other interfaces, so this runs for interfaces too rather than as an else-branch.
	for _, nt := range all {
		if nt.named == target {
			continue
		}
		iface, isIface := nt.named.Underlying().(*types.Interface)
		if !isIface || iface.NumMethods() == 0 {
			continue
		}
		switch {
		case types.Implements(target, iface):
			res.Interfaces = append(res.Interfaces, s.implEntry(nt, "value"))
		case types.Implements(types.NewPointer(target), iface):
			res.Interfaces = append(res.Interfaces, s.implEntry(nt, "pointer"))
		}
	}
	res.Note = appendNote(res.Note, "scope: named types declared in this module, tests excluded")
	return res, nil
}

func (s *Snapshot) implEntry(nt namedType, recv string) ImplEntry {
	return ImplEntry{
		Name:     nt.pkg.PkgPath + "." + nt.obj.Name(),
		Kind:     kindOf(nt.obj),
		Receiver: recv,
		Location: s.anchor(nt.obj.Pos()),
	}
}

// defaultSymbolCap and maxSymbolCap bound an outline.
const (
	defaultSymbolCap = 200
	maxSymbolCap     = 1000
)

func (ix *index) Symbols(ctx context.Context, req Request) (*SymbolsResult, error) {
	limit := req.Max
	if limit <= 0 {
		limit = defaultSymbolCap
	}
	if limit > maxSymbolCap {
		limit = maxSymbolCap
	}
	var out *SymbolsResult
	err := ix.withSnapshot(ctx, req, func(s *Snapshot) ([]string, error) {
		target := strings.TrimSpace(req.Target)
		if target == "" {
			target = req.Dir
		}
		if target == "" {
			target = "."
		}
		if p := s.packageForPath(target); p != nil {
			file := ""
			if strings.HasSuffix(target, ".go") {
				file = s.absFile(target)
			}
			out = s.outline(p, file, target, limit)
			return []string{p.PkgPath}, nil
		}
		matched := s.matchPackages(target)
		switch len(matched) {
		case 0:
			return nil, wrapRecoverable(ErrNotFound,
				"no package or file named %s in this module; pass an import path, a package name, or a workspace-relative .go file",
				echoArg(target))
		case 1:
			out = s.outline(matched[0], "", target, limit)
			return []string{matched[0].PkgPath}, nil
		}
		names := make([]string, 0, len(matched))
		for _, p := range matched {
			names = append(names, p.PkgPath)
		}
		if len(names) > 8 {
			names = append(append([]string(nil), names[:8]...), fmt.Sprintf("+%d more", len(matched)-8))
		}
		return nil, wrapRecoverable(ErrAmbiguous,
			"%s matches %d packages: %s; re-ask with one full import path",
			echoArg(target), len(matched), strings.Join(names, ", "))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Snapshot) absFile(target string) string {
	p := s.packageForPath(target)
	if p == nil {
		return ""
	}
	for _, f := range p.GoFiles {
		if displayPath(s.Base, f) == strings.TrimPrefix(target, "./") || f == target {
			return f
		}
	}
	return ""
}

// outline walks the package's own syntax rather than its type scope, so entries come out with source attribution and methods appear as "Type.Method" without a second pass over every named type.
func (s *Snapshot) outline(p *packages.Package, file, target string, limit int) *SymbolsResult {
	kind := "package"
	if file != "" {
		kind = "file"
	}
	res := &SymbolsResult{
		Target:    p.PkgPath,
		Kind:      kind,
		Toolchain: s.Toolchain.String(),
	}
	if kind == "file" {
		res.Target = displayPath(s.Base, file)
	}
	qual := types.RelativeTo(p.Types)

	var all []Symbol
	add := func(name, fallbackKind string, obj types.Object, pos ast.Node) {
		sym := Symbol{Name: name, Kind: fallbackKind, Location: s.anchor(pos.Pos())}
		if obj != nil {
			sym.Kind = kindOf(obj)
			sym.Type = types.TypeString(obj.Type(), qual)
		}
		all = append(all, sym)
	}

	for _, f := range p.Syntax {
		if file != "" && s.Fset.Position(f.Pos()).Filename != file {
			continue
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				name := d.Name.Name
				fallback := "func"
				if d.Recv != nil && len(d.Recv.List) > 0 {
					if recv := receiverTypeName(d.Recv.List[0].Type); recv != "" {
						name = recv + "." + name
					}
					fallback = "method"
				}
				var obj types.Object
				if p.TypesInfo != nil {
					obj = p.TypesInfo.Defs[d.Name]
				}
				add(name, fallback, obj, d.Name)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						var obj types.Object
						if p.TypesInfo != nil {
							obj = p.TypesInfo.Defs[sp.Name]
						}
						add(sp.Name.Name, "type", obj, sp.Name)
					case *ast.ValueSpec:
						fallback := "var"
						if d.Tok.String() == "const" {
							fallback = "const"
						}
						for _, n := range sp.Names {
							var obj types.Object
							if p.TypesInfo != nil {
								obj = p.TypesInfo.Defs[n]
							}
							add(n.Name, fallback, obj, n)
						}
					}
				}
			}
		}
	}

	// Exported first, then by name, so two runs against the same source produce the same bytes.
	sort.SliceStable(all, func(i, j int) bool {
		ei, ej := isExportedSymbol(all[i].Name), isExportedSymbol(all[j].Name)
		if ei != ej {
			return ei
		}
		return all[i].Name < all[j].Name
	})

	res.Total = len(all)
	if len(all) > limit {
		res.Note = appendNote(res.Note, fmt.Sprintf("+%d more symbols not shown; raise max (ceiling %d) or outline a single file", len(all)-limit, maxSymbolCap))
		all = all[:limit]
	}
	res.Symbols = all
	res.Shown = len(all)
	if res.Symbols == nil {
		res.Symbols = []Symbol{}
	}
	res.Note = appendNote(res.Note, "tests excluded")
	return res
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver: Type[T]
		return receiverTypeName(t.X)
	case *ast.IndexListExpr:
		return receiverTypeName(t.X)
	}
	return ""
}

func isExportedSymbol(name string) bool {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		// A method is API only if both its receiver and its own name are exported.
		return isExportedSymbol(name[:i]) && isExportedSymbol(name[i+1:])
	}
	return name != "" && name[0] >= 'A' && name[0] <= 'Z'
}

func kindOf(obj types.Object) string {
	switch o := obj.(type) {
	case *types.PkgName:
		return "package"
	case *types.Const:
		return "const"
	case *types.TypeName:
		if o.IsAlias() {
			return "type alias"
		}
		switch o.Type().Underlying().(type) {
		case *types.Interface:
			return "interface"
		case *types.Struct:
			return "struct"
		}
		return "type"
	case *types.Var:
		if o.IsField() {
			return "field"
		}
		return "var"
	case *types.Func:
		if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
			return "method"
		}
		return "func"
	case *types.Builtin:
		return "builtin"
	case *types.Label:
		return "label"
	case *types.Nil:
		return "nil"
	}
	return "object"
}

// declPath returns the AST path enclosing an object's declaration, or nil when the object was not declared in this module's syntax.
func (s *Snapshot) declPath(ref symbolRef) []ast.Node {
	pos := ref.obj.Pos()
	if !pos.IsValid() || ref.pkg == nil {
		return nil
	}
	for _, f := range ref.pkg.Syntax {
		if f.Pos() <= pos && pos <= f.End() {
			path, _ := astutil.PathEnclosingInterval(f, pos, pos)
			return path
		}
	}
	return nil
}

// docFor returns the doc comment attached to an object's declaration. The innermost carrier wins (a field's own comment beats the struct's), with the enclosing GenDecl as the fallback so `// Foo does X` above a single-spec `var Foo = ...` is still found.
func (s *Snapshot) docFor(ref symbolRef) string {
	var fallback *ast.CommentGroup
	for _, n := range s.declPath(ref) {
		switch d := n.(type) {
		case *ast.Field:
			if d.Doc != nil {
				return commentText(d.Doc)
			}
		case *ast.ValueSpec:
			if d.Doc != nil {
				return commentText(d.Doc)
			}
		case *ast.TypeSpec:
			if d.Doc != nil {
				return commentText(d.Doc)
			}
		case *ast.FuncDecl:
			if d.Doc != nil {
				return commentText(d.Doc)
			}
		case *ast.GenDecl:
			if d.Doc != nil && fallback == nil {
				fallback = d.Doc
			}
		}
	}
	if fallback != nil {
		return commentText(fallback)
	}
	return ""
}

// signatureSource renders a function's declaration exactly as written, minus the body — the form an agent would type at a call site.
func (s *Snapshot) signatureSource(ref symbolRef) string {
	for _, n := range s.declPath(ref) {
		fd, ok := n.(*ast.FuncDecl)
		if !ok {
			continue
		}
		clone := *fd
		clone.Body = nil
		clone.Doc = nil
		var b strings.Builder
		if err := printer.Fprint(&b, s.Fset, &clone); err != nil {
			return ""
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func commentText(g *ast.CommentGroup) string {
	return strings.TrimSpace(g.Text())
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func clampRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

func appendNote(existing, add string) string {
	if add == "" {
		return existing
	}
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

func sortMembers(m []Member) {
	sort.SliceStable(m, func(i, j int) bool {
		ei, ej := isExportedSymbol(m[i].Name), isExportedSymbol(m[j].Name)
		if ei != ej {
			return ei
		}
		return m[i].Name < m[j].Name
	})
}
