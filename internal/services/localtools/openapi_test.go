package localtools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type schemaRepo interface {
	GetToolsForToolsByName(context.Context, string) ([]taskengine.Tool, error)
	GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error)
}

func assertPublishedDoc(t *testing.T, repo schemaRepo, provider string, components map[string]string) *openapi3.T {
	t.Helper()
	ctx := context.Background()

	docs, err := repo.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	doc, ok := docs[provider]
	require.Truef(t, ok, "the toolset publishes its contract under %q, got %v", provider, docs)
	assert.Equal(t, "3.1.0", doc.OpenAPI)
	require.NotNil(t, doc.Info)
	assert.NotEmpty(t, doc.Info.Title)
	assert.NotEmpty(t, doc.Info.Description)
	assert.NotEmpty(t, doc.Info.Version)
	require.NotNil(t, doc.Components)
	require.NoError(t, doc.Validate(ctx),
		"the published document is a valid OpenAPI document, not a shape that only looks like one")

	declared, err := repo.GetToolsForToolsByName(ctx, provider)
	require.NoError(t, err)
	require.Lenf(t, declared, len(components), "every declared tool needs a component prefix")
	assert.Lenf(t, doc.Components.Schemas, 2*len(components),
		"want a request and a response for each of the %d tools", len(components))

	for _, tool := range declared {
		name := tool.Function.Name
		component, ok := components[name]
		require.Truef(t, ok, "%s declares no OpenAPI component prefix", name)

		req := doc.Components.Schemas[component+"Request"]
		require.NotNilf(t, req, "%s: no %sRequest schema is published", name, component)
		require.NotNil(t, req.Value)
		resp := doc.Components.Schemas[component+"Response"]
		require.NotNilf(t, resp, "%s: no %sResponse schema is published", name, component)
		require.NotNil(t, resp.Value)
		assert.NotEmptyf(t, describedSchema(resp.Value), "%s: the response schema says nothing", name)

		params, ok := tool.Function.Parameters.(map[string]any)
		require.Truef(t, ok, "%s: parameters are %T, want a JSON Schema object", name, tool.Function.Parameters)
		props, _ := params["properties"].(map[string]any)
		assert.Lenf(t, req.Value.Properties, len(props),
			"%s: descriptor declares %d properties, the published schema %d", name, len(props), len(req.Value.Properties))

		for prop, published := range req.Value.Properties {
			declaredProp, ok := props[prop].(map[string]any)
			require.Truef(t, ok, "%s: %s is published but the descriptor does not declare it", name, prop)
			assert.NotEmptyf(t, published.Value.Description, "%s.%s is published without a description", name, prop)
			assert.Equalf(t, declaredProp["description"], published.Value.Description,
				"%s.%s: descriptor and published schema must carry the same description", name, prop)
			assert.ElementsMatchf(t, asAnySlice(declaredProp["enum"]), published.Value.Enum,
				"%s.%s: descriptor and published schema must declare the same closed value set", name, prop)
			switch want := declaredProp["type"].(type) {
			case string:
				require.NotNilf(t, published.Value.Type, "%s.%s is published without a type", name, prop)
				assert.Truef(t, published.Value.Type.Is(want), "%s.%s: published type %v, descriptor type %v", name, prop, published.Value.Type, want)
			case []any:
				// A type union: "null" is rendered as nullable, since an OpenAPI validator knows no "null" type.
				require.NotNilf(t, published.Value.Type, "%s.%s is published without a type", name, prop)
				for _, one := range want {
					if one == "null" {
						assert.Truef(t, published.Value.Nullable, "%s.%s: the descriptor allows null", name, prop)
						continue
					}
					assert.Containsf(t, []string(*published.Value.Type), one, "%s.%s: published type %v drops %v", name, prop, published.Value.Type, one)
				}
			}
		}

		wantRequired, _ := params["required"].([]string)
		assert.ElementsMatchf(t, wantRequired, req.Value.Required, "%s: required set", name)
	}
	return doc
}

func describedSchema(s *openapi3.Schema) string {
	if s.Description != "" {
		return s.Description
	}
	for _, group := range [][]*openapi3.SchemaRef{s.OneOf, s.AnyOf} {
		for _, r := range group {
			if r != nil && r.Value != nil && r.Value.Description != "" {
				return r.Value.Description
			}
		}
	}
	return ""
}

func asAnySlice(v any) []any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		return x
	case []string:
		out := make([]any, 0, len(x))
		for _, s := range x {
			out = append(out, s)
		}
		return out
	}
	return nil
}

func assertEveryPropertyDescribed(t *testing.T, where string, s *openapi3.Schema) {
	t.Helper()
	if s == nil {
		return
	}
	for name, prop := range s.Properties {
		require.NotNilf(t, prop.Value, "%s.%s has no schema", where, name)
		assert.NotEmptyf(t, prop.Value.Description, "%s.%s is published without a description", where, name)
		if prop.Value.Type == nil {
			assert.Emptyf(t, prop.Value.Properties, "%s.%s is untyped but declares properties", where, name)
			assert.Nilf(t, prop.Value.Items, "%s.%s is untyped but declares items", where, name)
			assert.Emptyf(t, prop.Value.OneOf, "%s.%s is untyped but declares variants", where, name)
			assert.Emptyf(t, prop.Value.AnyOf, "%s.%s is untyped but declares variants", where, name)
			continue
		}
		assertEveryPropertyDescribed(t, where+"."+name, prop.Value)
	}
	if s.Items != nil {
		assertEveryPropertyDescribed(t, where+"[]", s.Items.Value)
	}
	for _, group := range [][]*openapi3.SchemaRef{s.OneOf, s.AnyOf, s.AllOf} {
		for i, r := range group {
			if r != nil {
				assertEveryPropertyDescribed(t, fmt.Sprintf("%s|%d", where, i), r.Value)
			}
		}
	}
}

func assertResultIsDeclared(t *testing.T, where string, schema *openapi3.Schema, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	for name := range got {
		assert.Containsf(t, schema.Properties, name,
			"%s: the result carries %s but the published schema does not declare it", where, name)
	}
	for _, name := range schema.Required {
		assert.Containsf(t, got, name,
			"%s: the published schema requires %s but the result omits it", where, name)
	}
}

func variantByRequired(t *testing.T, s *openapi3.Schema, key string) *openapi3.Schema {
	t.Helper()
	for _, group := range [][]*openapi3.SchemaRef{s.OneOf, s.AnyOf} {
		for _, r := range group {
			if r == nil || r.Value == nil {
				continue
			}
			for _, req := range r.Value.Required {
				if req == key {
					return r.Value
				}
			}
		}
	}
	t.Fatalf("no published variant requires %q", key)
	return nil
}

var fsComponents = map[string]string{
	"read_file":       "LocalFsReadFile",
	"write_file":      "LocalFsWriteFile",
	"edit_file":       "LocalFsEditFile",
	"list_dir":        "LocalFsListDir",
	"grep":            "LocalFsGrep",
	"find_files":      "LocalFsFindFiles",
	"sed":             "LocalFsSed",
	"count_stats":     "LocalFsCountStats",
	"read_file_range": "LocalFsReadFileRange",
	"stat_file":       "LocalFsStatFile",
}

func TestUnit_LocalFSTools_PublishedSchemaMatchesToolDescriptors(t *testing.T) {
	dir := t.TempDir()
	repo := localtools.NewLocalFSTools(dir, nil).(schemaRepo)
	doc := assertPublishedDoc(t, repo, "local_fs", fsComponents)

	for _, component := range fsComponents {
		assertEveryPropertyDescribed(t, component, doc.Components.Schemas[component+"Response"].Value)
	}

	ctx := context.Background()
	now := time.Now()
	exec := func(tool string, args map[string]any) any {
		t.Helper()
		out, _, err := repo.(taskengine.ToolsRepo).Exec(ctx, now, args, false, &taskengine.ToolsCall{ToolName: tool})
		require.NoErrorf(t, err, "%s", tool)
		return out
	}

	written := exec("write_file", map[string]any{"path": "a.txt", "content": "one\ntwo\n"})
	assertResultIsDeclared(t, "write_file",
		variantByRequired(t, doc.Components.Schemas["LocalFsWriteFileResponse"].Value, "written"), written)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644))
	stat := exec("stat_file", map[string]any{"path": "b.txt"})
	var statGot map[string]any
	require.NoError(t, json.Unmarshal([]byte(stat.(string)), &statGot))
	assertResultIsDeclared(t, "stat_file", doc.Components.Schemas["LocalFsStatFileResponse"].Value, statGot)

	found := exec("find_files", map[string]any{"pattern": "*.txt"})
	var foundGot map[string]any
	require.NoError(t, json.Unmarshal([]byte(found.(string)), &foundGot))
	assertResultIsDeclared(t, "find_files", doc.Components.Schemas["LocalFsFindFilesResponse"].Value, foundGot)
}

// TestUnit_LocalFSTools_MutatingResultsCarryTheDisplayPath pins that a local_fs result addresses a file as workspace-relative Path, while AbsPath (unserialized) survives for ToolDiff alone, since ACP tool-call locations are absolute by protocol.
func TestUnit_LocalFSTools_MutatingResultsCarryTheDisplayPath(t *testing.T) {
	dir := t.TempDir()
	repo := localtools.NewLocalFSTools(dir, nil)
	ctx := context.Background()
	now := time.Now()
	exec := func(tool string, args map[string]any) any {
		t.Helper()
		out, _, err := repo.Exec(ctx, now, args, false, &taskengine.ToolsCall{ToolName: tool})
		require.NoErrorf(t, err, "%s", tool)
		return out
	}

	// A nested path, so a relative form cannot be mistaken for a bare basename.
	const rel = "pkg/a.txt"
	abs := filepath.Join(dir, "pkg", "a.txt")

	write := exec("write_file", map[string]any{"path": rel, "content": "one\ntwo\n"}).(localtools.FsWriteResult)
	edit := exec("edit_file", map[string]any{"path": rel, "old_string": "one", "new_string": "ONE"}).(localtools.FsEditResult)
	sed := exec("sed", map[string]any{"path": rel, "pattern": "two", "replacement": "TWO"}).(localtools.FsSedResult)

	for _, tc := range []struct {
		tool   string
		result interface {
			ToolDiff() (string, string, string, bool)
		}
		path string
	}{
		{"write_file", write, write.Path},
		{"edit_file", edit, edit.Path},
		{"sed", sed, sed.Path},
	} {
		assert.Equalf(t, rel, tc.path, "%s: the result path must be workspace-relative like every other one", tc.tool)
		assert.NotContainsf(t, tc.path, dir, "%s: the host-absolute path must not reach the model", tc.tool)

		raw, err := json.Marshal(tc.result)
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.Equalf(t, rel, got["path"], "%s: the serialized path is the display form", tc.tool)

		diffPath, _, _, ok := tc.result.ToolDiff()
		require.Truef(t, ok, "%s: a write that changed the file produces a diff", tc.tool)
		assert.Equalf(t, abs, diffPath, "%s: ToolDiff must stay absolute — ACP locations are", tc.tool)
	}

	// A result built outside this package has no AbsPath; ToolDiff falls back to Path rather than losing the diff.
	fallback := localtools.FsWriteResult{Path: abs, Written: true, OldText: "a", NewText: "b"}
	diffPath, _, _, ok := fallback.ToolDiff()
	require.True(t, ok)
	assert.Equal(t, abs, diffPath)

	docs, err := repo.(schemaRepo).GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	for _, component := range []string{"LocalFsWriteFile", "LocalFsEditFile", "LocalFsSed"} {
		variant := variantByRequired(t, docs["local_fs"].Components.Schemas[component+"Response"].Value, "written")
		assert.Containsf(t, variant.Properties["path"].Value.Description, "relative to the project root",
			"%s: the published response must declare the path form the result actually carries", component)
	}
}

// TestUnit_LocalFSTools_PublishedSchemaTracksVerboseDescriptions pins that context-dependent descriptions (verbose mode) still match between the descriptor and the published schema.
func TestUnit_LocalFSTools_PublishedSchemaTracksVerboseDescriptions(t *testing.T) {
	repo := localtools.NewLocalFSTools(t.TempDir(), nil).(schemaRepo)
	ctx := taskengine.WithToolsArgs(context.Background(), "local_fs",
		map[string]string{"_verbose_tool_descriptions": "true"})
	assertPublishedDoc(t, repo, "local_fs", fsComponents)

	docs, err := repo.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	require.NoError(t, docs["local_fs"].Validate(ctx))
	declared, err := repo.GetToolsForToolsByName(ctx, "local_fs")
	require.NoError(t, err)
	for _, tool := range declared {
		params := tool.Function.Parameters.(map[string]any)
		props := params["properties"].(map[string]any)
		published := docs["local_fs"].Components.Schemas[fsComponents[tool.Function.Name]+"Request"]
		for prop, want := range props {
			assert.Equalf(t, want.(map[string]any)["description"], published.Value.Properties[prop].Value.Description,
				"%s.%s: the verbose descriptor and the published schema must agree", tool.Function.Name, prop)
		}
	}
}

var gitComponents = map[string]string{
	"git_status":          "GitStatus",
	"git_diff":            "GitDiff",
	"git_log":             "GitLog",
	"git_show":            "GitShow",
	"git_branch_list":     "GitBranchList",
	"git_blame":           "GitBlame",
	"git_add":             "GitAdd",
	"git_commit":          "GitCommit",
	"git_checkout_branch": "GitCheckoutBranch",
	"git_restore":         "GitRestore",
}

func TestUnit_GitTools_PublishedSchemaMatchesToolDescriptors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	newTestRepo(t, dir, map[string]string{"a.txt": "one\n"})
	repo := localtools.NewGitTools(dir).(schemaRepo)
	doc := assertPublishedDoc(t, repo, "git", gitComponents)

	for _, component := range gitComponents {
		assertEveryPropertyDescribed(t, component, doc.Components.Schemas[component+"Response"].Value)
	}

	// The three tools that answer with a typed value are declared against what that value actually carries.
	ctx := context.Background()
	now := time.Now()
	exec := func(tool string, args map[string]any) any {
		t.Helper()
		out, _, err := repo.(taskengine.ToolsRepo).Exec(ctx, now, args, false, &taskengine.ToolsCall{ToolName: tool})
		require.NoErrorf(t, err, "%s", tool)
		return out
	}
	assertResultIsDeclared(t, "git_status", doc.Components.Schemas["GitStatusResponse"].Value, exec("git_status", map[string]any{}))
	assertResultIsDeclared(t, "git_log", doc.Components.Schemas["GitLogResponse"].Value, exec("git_log", map[string]any{}))
	assertResultIsDeclared(t, "git_branch_list", doc.Components.Schemas["GitBranchListResponse"].Value, exec("git_branch_list", map[string]any{}))

	// The seven text answers are declared as text, not as an object nobody returns.
	for _, tool := range []string{"git_diff", "git_show", "git_blame", "git_add", "git_commit", "git_checkout_branch", "git_restore"} {
		resp := doc.Components.Schemas[gitComponents[tool]+"Response"].Value
		require.NotNilf(t, resp.Type, "%s: response has no type", tool)
		assert.Truef(t, resp.Type.Is(openapi3.TypeString), "%s: response type %v", tool, resp.Type)
		assert.NotEmptyf(t, resp.Description, "%s: response is not described", tool)
	}
}

var webComponents = map[string]string{
	"web_get":    "WebGet",
	"web_head":   "WebHead",
	"web_post":   "WebPost",
	"web_put":    "WebPut",
	"web_patch":  "WebPatch",
	"web_delete": "WebDelete",
}

func TestUnit_WebCaller_PublishedSchemaMatchesToolDescriptors(t *testing.T) {
	t.Parallel()
	repo := localtools.NewWebCaller(nil).(schemaRepo)
	doc := assertPublishedDoc(t, repo, "webtools", webComponents)

	// The two argument shapes a flat property table could not have held survive the conversion.
	post := doc.Components.Schemas["WebPostRequest"].Value
	headers := post.Properties["headers"].Value
	require.NotNil(t, headers.AdditionalProperties.Schema, "headers keeps its additionalProperties")
	assert.True(t, headers.AdditionalProperties.Schema.Value.Type.Is(openapi3.TypeString))
	body := post.Properties["body"].Value
	assert.True(t, body.Nullable, "body accepts null")
	for _, want := range []string{"string", "number", "integer", "boolean", "object", "array"} {
		assert.Containsf(t, []string(*body.Type), want, "body keeps its %s branch", want)
	}
	require.NotNil(t, body.Items, "an array branch needs an item schema to be a valid document")

	// GET/HEAD take no body; the other four do — that difference is the contract.
	assert.NotContains(t, doc.Components.Schemas["WebGetRequest"].Value.Properties, "body")
	assert.NotContains(t, doc.Components.Schemas["WebHeadRequest"].Value.Properties, "body")

	for _, component := range webComponents {
		resp := doc.Components.Schemas[component+"Response"].Value
		assert.NotEmptyf(t, resp.AnyOf, "%s: the response shapes are not declared", component)
		assertEveryPropertyDescribed(t, component, resp)
	}
	envelope := variantByRequired(t, doc.Components.Schemas["WebGetResponse"].Value, "_truncated")
	for _, key := range []string{"_truncated", "_bytes_read", "_max_bytes", "body"} {
		assert.Containsf(t, envelope.Properties, key, "the truncation envelope declares %s", key)
	}

	// The descriptor must also say what only the schema declares (non-2xx is an error, an oversized body changes the shape), since that is all a model has at call time.
	declared, err := repo.GetToolsForToolsByName(context.Background(), "webtools")
	require.NoError(t, err)
	require.Len(t, declared, len(webComponents))
	for _, tool := range declared {
		desc := tool.Function.Description
		assert.Containsf(t, desc, "non-2xx status is returned as an ERROR",
			"%s: a model cannot tell a 404 from a transport failure unless the descriptor says so", tool.Function.Name)
		if tool.Function.Name == "web_head" {
			assert.NotContains(t, desc, "_truncated", "web_head has no body to truncate")
			continue
		}
		assert.Containsf(t, desc, "{_truncated,_bytes_read,_max_bytes,body}",
			"%s: the truncation envelope changes the result shape, so the descriptor must name it", tool.Function.Name)
	}
}
