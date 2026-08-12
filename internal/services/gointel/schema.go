package gointel

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

const buildContextNote = " Indexes the Go module containing dir (default: workspace root); host GOOS/GOARCH, no build tags, tests excluded."

var goToolDocs = map[string]string{
	ToolDescribe: "Type, signature, doc comment and — for named types — fields and methods of a Go symbol. " +
		"Name it as \"pkg.Ident\", \"pkg.Type.Method\", or a bare \"Ident\"; an ambiguous name is refused with the qualified candidates listed. " +
		"Use this instead of guessing an API or reading a whole file to find one declaration." + buildContextNote,

	ToolDefinition: "Where a Go symbol is declared: file:line:col plus the declaring source line. " +
		"Paths are workspace-relative, so the answer can be passed straight to a file-read tool." + buildContextNote,

	ToolReferences: "Every use of a Go symbol in this module, grouped by file with line numbers and one-line snippets. " +
		"Resolved by type identity, not text, so a same-named symbol in another package is never counted. " +
		"Capped at 50 results by default (max, ceiling 200); the result says how many were withheld." + buildContextNote,

	ToolImplementations: "Both directions of the implements relation: the module types implementing an interface, and the module interfaces a concrete type satisfies. " +
		"Pointer-only implementers are reported as such." + buildContextNote,

	ToolSymbols: "Outline of a package or a single .go file: every declaration, kind-tagged, exported first, each with file:line. " +
		"Cheaper than reading the file when you need to know what exists." + buildContextNote,

	ToolDiagnostics: "Type/parse errors plus vet passes (default: printf, unusedresult, unreachable, nilfunc) for scope \"changed\" (default: packages seen to change this session), \"package\", or \"all\". " +
		"The noisy-but-real \"shadow\" pass is opt-in via passes. " +
		"ADVISORY, NOT A BUILD: produced by this binary's type checker, which may differ from the repo's toolchain, so a finding is a strong signal and `go build` is the arbiter. Every result names the toolchain view it was produced under." + buildContextNote,
}

type goProperty struct {
	name        string
	types       []string
	itemType    string
	enum        []string
	description string
	required    bool
}

func (p goProperty) jsonType() any {
	if len(p.types) == 1 {
		return p.types[0]
	}
	out := make([]any, 0, len(p.types))
	for _, t := range p.types {
		out = append(out, t)
	}
	return out
}

func (p goProperty) parameter() map[string]any {
	out := map[string]any{"type": p.jsonType(), "description": p.description}
	if p.itemType != "" {
		items := map[string]any{"type": p.itemType}
		if len(p.enum) > 0 {
			items["enum"] = append([]string(nil), p.enum...)
		}
		out["items"] = items
		return out
	}
	if len(p.enum) > 0 {
		out["enum"] = append([]string(nil), p.enum...)
	}
	return out
}

func (p goProperty) schema() *openapi3.SchemaRef {
	types := openapi3.Types(append([]string(nil), p.types...))
	s := &openapi3.Schema{Type: &types, Description: p.description}
	if p.itemType != "" {
		items := &openapi3.Schema{Type: &openapi3.Types{p.itemType}}
		for _, v := range p.enum {
			items.Enum = append(items.Enum, v)
		}
		s.Items = &openapi3.SchemaRef{Value: items}
		return &openapi3.SchemaRef{Value: s}
	}
	for _, v := range p.enum {
		s.Enum = append(s.Enum, v)
	}
	return &openapi3.SchemaRef{Value: s}
}

type goToolSpec struct {
	name        string
	component   string
	description string
	props       []goProperty
	response    func() *openapi3.SchemaRef
}

func goToolSpecs() []goToolSpec {
	return []goToolSpec{
		{
			name:        ToolDescribe,
			component:   "GoDescribe",
			description: goToolDocs[ToolDescribe],
			props:       []goProperty{symbolProperty(), dirProperty()},
			response:    describeResponseSchema,
		},
		{
			name:        ToolDefinition,
			component:   "GoDefinition",
			description: goToolDocs[ToolDefinition],
			props:       []goProperty{symbolProperty(), dirProperty()},
			response:    definitionResponseSchema,
		},
		{
			name:        ToolReferences,
			component:   "GoReferences",
			description: goToolDocs[ToolReferences],
			props: []goProperty{
				symbolProperty(),
				maxProperty("references", defaultRefCap, maxRefCap),
				dirProperty(),
			},
			response: referencesResponseSchema,
		},
		{
			name:        ToolImplementations,
			component:   "GoImplementations",
			description: goToolDocs[ToolImplementations],
			props:       []goProperty{symbolProperty(), dirProperty()},
			response:    implementationsResponseSchema,
		},
		{
			name:        ToolSymbols,
			component:   "GoSymbols",
			description: goToolDocs[ToolSymbols],
			props: []goProperty{
				{name: "target", types: []string{"string"}, description: "Package (import path, path suffix, or package name) or a workspace-relative .go file. Defaults to the package at dir."},
				maxProperty("symbols", defaultSymbolCap, maxSymbolCap),
				dirProperty(),
			},
			response: symbolsResponseSchema,
		},
		{
			name:        ToolDiagnostics,
			component:   "GoDiagnostics",
			description: goToolDocs[ToolDiagnostics],
			props: []goProperty{
				{
					name:        "scope",
					types:       []string{"string"},
					enum:        []string{ScopeChanged, ScopePackage, ScopeAll},
					description: "\"changed\" (default), \"package\", or \"all\".",
				},
				{name: "target", types: []string{"string"}, description: "Package or workspace-relative .go file, for scope \"package\"."},
				passesProperty(),
				maxProperty("diagnostics", defaultDiagCap, maxDiagCap),
				dirProperty(),
			},
			response: diagnosticsResponseSchema,
		},
	}
}

func dirProperty() goProperty {
	return goProperty{
		name:        "dir",
		types:       []string{"string"},
		description: "Directory inside the target module, relative to the workspace root (default: workspace root). Only needed when the workspace holds more than one Go module.",
	}
}

func symbolProperty() goProperty {
	return goProperty{
		name:        "symbol",
		types:       []string{"string"},
		description: "Qualified symbol: \"pkg.Ident\", \"pkg.Type.Method\", or a bare \"Ident\". pkg may be a package name, an import-path suffix, or a full import path.",
		required:    true,
	}
}

func maxProperty(unit string, def, ceiling int) goProperty {
	return goProperty{
		name:        "max",
		types:       []string{"integer"},
		description: fmt.Sprintf("Maximum %s to return (default %d, ceiling %d). A decimal string is read as the number it spells.", unit, def, ceiling),
	}
}

func passesProperty() goProperty {
	return goProperty{
		name:     "passes",
		types:    []string{"string", "array"},
		itemType: "string",
		enum:     append(VetPasses(), "all"),
		description: fmt.Sprintf("Vet passes to run, as an array of names or one comma-separated string: %s, or \"all\". Default: %s.",
			strings.Join(VetPasses(), ", "), strings.Join(DefaultVetPasses(), ", ")),
	}
}

func (s goToolSpec) required() []string {
	var out []string
	for _, p := range s.props {
		if p.required {
			out = append(out, p.name)
		}
	}
	return out
}

func (s goToolSpec) parameters() map[string]any {
	props := make(map[string]any, len(s.props))
	for _, p := range s.props {
		props[p.name] = p.parameter()
	}
	params := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if required := s.required(); len(required) > 0 {
		params["required"] = required
	}
	return params
}

func (s goToolSpec) requestSchema() *openapi3.SchemaRef {
	props := make(map[string]*openapi3.SchemaRef, len(s.props))
	for _, p := range s.props {
		props[p.name] = p.schema()
	}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:       &openapi3.Types{openapi3.TypeObject},
		Properties: props,
		Required:   s.required(),
	}}
}

func (s goToolSpec) tool() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        s.name,
			Description: s.description,
			Parameters:  s.parameters(),
		},
	}
}

func (h *tools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	specs := goToolSpecs()
	allTools := make([]taskengine.Tool, 0, len(specs))
	for _, spec := range specs {
		allTools = append(allTools, spec.tool())
	}

	if name == ToolsProviderName || name == "" {
		return allTools, nil
	}
	for _, t := range allTools {
		if t.Function.Name == name {
			return []taskengine.Tool{t}, nil
		}
	}
	return nil, fmt.Errorf("gointel: unknown tool %q", name)
}

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract, one request/response pair per declared tool.
func (h *tools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	specs := goToolSpecs()
	schemas := make(map[string]*openapi3.SchemaRef, 2*len(specs))
	for _, spec := range specs {
		schemas[spec.component+"Request"] = spec.requestSchema()
		schemas[spec.component+"Response"] = spec.response()
	}
	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "Go Intelligence Tools",
			Description: "Type-checked answers about the Go code in the workspace: describe, definition, references, implementations, outlines and advisory diagnostics. Every tool is a pure read of an in-memory snapshot — no process is spawned, nothing is written, nothing leaves the workspace." + buildContextNote,
			Version:     "1.0.0",
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: schemas,
		},
	}
	return map[string]*openapi3.T{ToolsProviderName: schema}, nil
}

func stringProp(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: description}}
}

func intProp(description string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeInteger}, Description: description}}
}

func arrayProp(description string, items *openapi3.SchemaRef) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:        &openapi3.Types{openapi3.TypeArray},
		Items:       items,
		Description: description,
	}}
}

func objectSchema(props map[string]*openapi3.SchemaRef, required ...string) *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		Type:       &openapi3.Types{openapi3.TypeObject},
		Properties: props,
		Required:   required,
	}}
}

func toolchainProp() *openapi3.SchemaRef {
	return stringProp("The build context this answer was produced under (Go version, GOOS/GOARCH). A repo built on another toolchain can see a different picture.")
}

func noteProp(description string) *openapi3.SchemaRef {
	return stringProp(description)
}

func kindProp() *openapi3.SchemaRef {
	return stringProp("What the type checker says the symbol is: func, method, struct, interface, type, type alias, const, var, field, or package.")
}

func enumStringProp(description string, values ...string) *openapi3.SchemaRef {
	s := &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: description}
	for _, v := range values {
		s.Enum = append(s.Enum, v)
	}
	return &openapi3.SchemaRef{Value: s}
}

func memberSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"name":     stringProp("The field or method name."),
		"kind":     enumStringProp("Whether this member is a field or a method.", "field", "method"),
		"type":     stringProp("The field's type, or the method's signature."),
		"doc":      stringProp("The declaration's doc comment, when it has one."),
		"location": stringProp("Workspace-relative file:line:col of the declaration, when it is in this module."),
	}, "name", "kind", "type")
}

func describeResponseSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"symbol":     stringProp("The fully qualified symbol the name resolved to."),
		"kind":       kindProp(),
		"type":       stringProp("The symbol's type, when it has one."),
		"signature":  stringProp("The full signature, for a func or method."),
		"doc":        stringProp("The declaration's doc comment."),
		"location":   stringProp("Workspace-relative file:line:col of the declaration."),
		"underlying": stringProp("The underlying type, for a named type whose underlying type differs."),
		"fields":     arrayProp("The struct's fields, for a named struct type.", memberSchema()),
		"methods":    arrayProp("The type's methods, for a named type.", memberSchema()),
		"note":       noteProp("What was left out, e.g. \"+3 more methods\" when a large type's members were capped."),
		"toolchain":  toolchainProp(),
	}, "symbol", "kind", "location", "toolchain")
}

func definitionResponseSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"symbol":    stringProp("The fully qualified symbol the name resolved to."),
		"kind":      kindProp(),
		"location":  stringProp("Workspace-relative file:line:col of the declaration — ready to pass to a file-read tool."),
		"line":      stringProp("The declaring source line itself."),
		"module":    stringProp("The module path the declaration was resolved in."),
		"toolchain": toolchainProp(),
	}, "symbol", "kind", "location", "toolchain")
}

func refLineSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"line": intProp("The 1-based line number."),
		"text": stringProp("The source line, trimmed."),
		"uses": intProp("How many times the symbol occurs on this line; set only when it occurs more than once."),
	}, "line")
}

func refFileSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"file":  stringProp("Workspace-relative path of the file."),
		"count": intProp("How many distinct lines in this file use the symbol."),
		"lines": arrayProp("The using lines, in file order.", refLineSchema()),
	}, "file", "count", "lines")
}

func referencesResponseSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"symbol":     stringProp("The fully qualified symbol the name resolved to."),
		"definition": stringProp("Workspace-relative file:line:col where the symbol is declared."),
		"total":      intProp("Distinct file:line locations that use the symbol — what the cap applies to."),
		"uses":       intProp("Raw identifier occurrences, which exceeds total when a line uses the symbol more than once."),
		"shown":      intProp("How many locations this result carries; below total when a cap bit."),
		"files":      arrayProp("The using locations, grouped by file.", refFileSchema()),
		"note":       noteProp("What this answer covers and what it leaves out: the scope statement (this module's packages, tests excluded), how many locations were withheld and the ceiling to raise max to, and — for an interface method — that these are calls THROUGH the interface rather than uses of any implementation."),
		"toolchain":  toolchainProp(),
	}, "symbol", "definition", "total", "uses", "shown", "files", "toolchain")
}

func implEntrySchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"name":     stringProp("The fully qualified type or interface name."),
		"kind":     kindProp(),
		"receiver": enumStringProp("Which receiver form satisfies the interface: \"value\" (both forms do) or \"pointer\" (only the pointer type does).", "value", "pointer"),
		"location": stringProp("Workspace-relative file:line:col of the declaration."),
	}, "name", "kind", "location")
}

func implementationsResponseSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"symbol":       stringProp("The fully qualified symbol the name resolved to."),
		"kind":         kindProp(),
		"implementers": arrayProp("The module types implementing this interface — filled when the queried symbol is an interface.", implEntrySchema()),
		"interfaces":   arrayProp("The module interfaces the queried type satisfies. An interface is asked in both directions, so this can be filled alongside implementers.", implEntrySchema()),
		"note":         noteProp("What this answer covers: the scope statement (named types declared in this module, tests excluded), and \"no type in this module implements it\" when nothing does."),
		"toolchain":    toolchainProp(),
	}, "symbol", "kind", "toolchain")
}

func symbolEntrySchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"name":     stringProp("The declared name."),
		"kind":     kindProp(),
		"type":     stringProp("The type or signature, when it has one."),
		"location": stringProp("Workspace-relative file:line of the declaration."),
	}, "name", "kind", "location")
}

func symbolsResponseSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"target":    stringProp("What was outlined: the resolved package import path, or the workspace-relative file."),
		"kind":      enumStringProp("Whether the outline is of a package or a single file.", "package", "file"),
		"total":     intProp("How many declarations the target has."),
		"shown":     intProp("How many this result carries; below total when a cap bit."),
		"symbols":   arrayProp("The declarations, exported first.", symbolEntrySchema()),
		"note":      noteProp("What this answer covers and what it leaves out: that tests are excluded, plus how many declarations were withheld and the ceiling to raise max to."),
		"toolchain": toolchainProp(),
	}, "target", "kind", "total", "shown", "symbols", "toolchain")
}

func diagnosticSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"location": stringProp("Workspace-relative file:line:col the finding is about."),
		"severity": &openapi3.SchemaRef{Value: &openapi3.Schema{
			Type:        &openapi3.Types{openapi3.TypeString},
			Enum:        []any{"type-error", "vet"},
			Description: "\"type-error\" (the load's own parse/type error, nearly always real) or \"vet\" (a curated analysis pass, which is advice).",
		}},
		"category": stringProp("The analyzer name, or \"type\" for a load error."),
		"message":  stringProp("What the checker reported."),
		"line":     stringProp("The offending source line, when one could be read."),
	}, "location", "severity", "category", "message")
}

func diagnosticsResponseSchema() *openapi3.SchemaRef {
	return objectSchema(map[string]*openapi3.SchemaRef{
		"scope":        enumStringProp("The scope that was swept.", "changed", "package", "all"),
		"passes":       arrayProp("The analysis passes that actually ran, so a clean result cannot be mistaken for \"everything was checked\".", stringProp("A vet pass name.")),
		"packages":     arrayProp("The packages that were swept.", stringProp("A package import path.")),
		"type_errors":  intProp("How many findings are parse/type errors."),
		"vet_findings": intProp("How many findings come from a vet pass."),
		"total":        intProp("How many findings the sweep produced."),
		"shown":        intProp("How many this result carries; below total when a cap bit."),
		"diagnostics":  arrayProp("The findings.", diagnosticSchema()),
		"note":         noteProp("What this sweep covers and what it leaves out: the advisory statement (`go build` is the arbiter; tests excluded), \"clean\" when nothing was found, how many findings were withheld and the ceiling to raise max to, and how many packages were examined when the listing itself was capped."),
		"toolchain":    toolchainProp(),
	}, "scope", "passes", "packages", "type_errors", "vet_findings", "total", "shown", "diagnostics", "toolchain")
}
