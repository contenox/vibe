package gointel

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

// ---------------------------------------------------------------------------
// Tool schemas
//
// Terse by default, for the reason localtools/fs_schema.go documents at length:
// a description is paid on EVERY turn, while everything a long description
// pre-teaches is re-taught by the error message at the moment it fires, with the
// concrete symbol and path filled in. Six tools at 200 words each would be a
// four-figure token tax per turn before a single answer is read.
//
// Two things are stated in the schema anyway, because they cannot be re-taught
// by an error that never fires:
//
//   - the BUILD CONTEXT defaults (host GOOS/GOARCH, no build tags, tests
//     excluded), because a correct-looking answer produced under a different
//     build context is a silent wrong answer, not an error; and
//   - the ADVISORY framing on go_diagnostics, because a model that treats these
//     findings as a verdict will "fix" phantom errors.
// ---------------------------------------------------------------------------

// buildContextNote is the one-line build-context statement appended to every
// tool description. Verbatim-brief on purpose.
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

func goTool(name, desc string, props map[string]any, required ...string) taskengine.Tool {
	params := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		params["required"] = required
	}
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        name,
			Description: desc,
			Parameters:  params,
		},
	}
}

func goProp(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

// dirProp is the shared "which module" argument. Every tool takes it and every
// tool defaults it to the workspace root, so a single-module workspace never has
// to pass it.
func dirProp() map[string]any {
	return goProp("string", "Directory inside the target module, relative to the workspace root (default: workspace root). Only needed when the workspace holds more than one Go module.")
}

func symbolProp() map[string]any {
	return goProp("string", "Qualified symbol: \"pkg.Ident\", \"pkg.Type.Method\", or a bare \"Ident\". pkg may be a package name, an import-path suffix, or a full import path.")
}

func (h *tools) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	allTools := []taskengine.Tool{
		goTool(ToolDescribe, goToolDocs[ToolDescribe], map[string]any{
			"symbol": symbolProp(),
			"dir":    dirProp(),
		}, "symbol"),

		goTool(ToolDefinition, goToolDocs[ToolDefinition], map[string]any{
			"symbol": symbolProp(),
			"dir":    dirProp(),
		}, "symbol"),

		goTool(ToolReferences, goToolDocs[ToolReferences], map[string]any{
			"symbol": symbolProp(),
			"max":    goProp("integer", fmt.Sprintf("Maximum references to return (default %d, ceiling %d).", defaultRefCap, maxRefCap)),
			"dir":    dirProp(),
		}, "symbol"),

		goTool(ToolImplementations, goToolDocs[ToolImplementations], map[string]any{
			"symbol": symbolProp(),
			"dir":    dirProp(),
		}, "symbol"),

		goTool(ToolSymbols, goToolDocs[ToolSymbols], map[string]any{
			"target": goProp("string", "Package (import path, path suffix, or package name) or a workspace-relative .go file. Defaults to the package at dir."),
			"max":    goProp("integer", fmt.Sprintf("Maximum symbols to return (default %d, ceiling %d).", defaultSymbolCap, maxSymbolCap)),
			"dir":    dirProp(),
		}),

		goTool(ToolDiagnostics, goToolDocs[ToolDiagnostics], map[string]any{
			"scope":  goProp("string", "\"changed\" (default), \"package\", or \"all\"."),
			"target": goProp("string", "Package or workspace-relative .go file, for scope \"package\"."),
			"passes": goProp("string", fmt.Sprintf("Comma-separated vet passes to run: %s, or \"all\". Default: %s.", strings.Join(VetPasses(), ", "), strings.Join(DefaultVetPasses(), ", "))),
			"max":    goProp("integer", fmt.Sprintf("Maximum diagnostics to return (default %d, ceiling %d).", defaultDiagCap, maxDiagCap)),
			"dir":    dirProp(),
		}),
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
