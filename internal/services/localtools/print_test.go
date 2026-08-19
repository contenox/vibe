package localtools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// printUniverse is what PersistentRepo.Supports hands the allowlist: this
// toolset's registered key alongside an unscoped operator registration and a
// declared MCP row.
func printUniverse() []string {
	return []string{localtools.PrintToolsName, "local_fs", "decl-reviewer-github"}
}

func printAdmits(allowlist []string, name string) bool {
	for _, got := range taskengine.ExportedApplyAllowlist(allowlist, printUniverse()) {
		if got == name {
			return true
		}
	}
	return false
}

func printCall(tool string, args map[string]string) *taskengine.ToolsCall {
	return &taskengine.ToolsCall{Name: localtools.PrintToolsName, ToolName: tool, Args: args}
}

func execPrint(t *testing.T, ctx context.Context, repo taskengine.ToolsRepo, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	t.Helper()
	return repo.Exec(ctx, time.Now(), input, false, call)
}

// TestUnit_PrintTools_Gate_StarAdmitsScopedNameBangRemovesIt pins the allowlist vocabulary: "*" admits every connected toolset with no exceptions, "!name" removes one, a bare name grants exactly it, an empty allowlist grants nothing.
func TestUnit_PrintTools_Gate_StarAdmitsScopedNameBangRemovesIt(t *testing.T) {
	t.Parallel()

	// native- is a namespace, so a declared MCP source cannot mint this key.
	require.Truef(t, strings.HasPrefix(localtools.PrintToolsName, "native-"),
		"toolset name %q dropped the native- namespace; a declared source could collide with it", localtools.PrintToolsName)

	assert.Truef(t, printAdmits([]string{"*"}, localtools.PrintToolsName),
		"%q must be admitted by \"*\": the scope is a namespace, not a hidden exclusion", localtools.PrintToolsName)
	assert.True(t, printAdmits([]string{"*"}, "local_fs"), "the wildcard no longer admits unscoped toolsets")
	assert.True(t, printAdmits([]string{"*"}, "decl-reviewer-github"),
		"the wildcard must admit a declared MCP row too; \"*\" means everything")

	assert.Truef(t, printAdmits([]string{localtools.PrintToolsName}, localtools.PrintToolsName),
		"%q is not admitted when a declaration names it exactly", localtools.PrintToolsName)
	assert.False(t, printAdmits([]string{localtools.PrintToolsName}, "local_fs"),
		"a bare name granted more than exactly itself")
	assert.Falsef(t, printAdmits([]string{"*", "!" + localtools.PrintToolsName}, localtools.PrintToolsName),
		"%q survives \"!\" under the wildcard; an operator cannot remove one set", localtools.PrintToolsName)
	assert.True(t, printAdmits([]string{"*", "!" + localtools.PrintToolsName}, "local_fs"),
		"removing one set removed the others with it")
	assert.Falsef(t, printAdmits([]string{localtools.PrintToolsName, "!" + localtools.PrintToolsName}, localtools.PrintToolsName),
		"%q survives its own denial entry", localtools.PrintToolsName)
	assert.Falsef(t, printAdmits(nil, localtools.PrintToolsName),
		"%q is admitted with no allowlist at all", localtools.PrintToolsName)
	// The pre-revival unscoped name is not a second door into the same toolset.
	assert.NotContains(t, printUniverse(), localtools.ToolPrint, "the unscoped registration key is back in the universe")
}

// The gate keys on the toolset name, so the name the policy block, the HITL
// rules and the registration all use must be the one Supports reports.
func TestUnit_PrintTools_RegisteredNameIsTheNameSupportsReports(t *testing.T) {
	t.Parallel()

	got, err := localtools.NewPrint(nil).Supports(context.Background())
	require.NoError(t, err)
	require.Equalf(t, []string{localtools.PrintToolsName}, got,
		"Supports() = %v, want exactly %q", got, localtools.PrintToolsName)

	// The tool itself is NOT prefixed: only toolset names reach the allowlist,
	// and "print" is the HITL policy key.
	assert.Falsef(t, strings.HasPrefix(localtools.ToolPrint, "native-"),
		"tool name %q is prefixed; the namespace scopes toolsets, not tools", localtools.ToolPrint)
}

// The descriptor is what an admitted toolset actually costs, so it is reachable
// under the registered key — the same key PersistentRepo dispatches on.
func TestUnit_PrintTools_DescriptorIsReachableUnderTheRegisteredKey(t *testing.T) {
	t.Parallel()
	repo := localtools.NewPrint(nil)
	ctx := context.Background()

	declared, err := repo.GetToolsForToolsByName(ctx, localtools.PrintToolsName)
	require.NoError(t, err)
	require.Len(t, declared, 1)
	assert.Equal(t, localtools.ToolPrint, declared[0].Function.Name)

	_, err = repo.GetToolsForToolsByName(ctx, localtools.ToolPrint)
	require.Error(t, err, "the pre-revival unscoped name must not resolve; it would be a second name no allowlist entry or policy block addresses")

	docs, err := repo.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	require.Contains(t, docs, localtools.PrintToolsName, "the contract is published under the registered key")
	require.NotContains(t, docs, localtools.ToolPrint)
}

func TestUnit_PrintTools_PublishesItsContract(t *testing.T) {
	t.Parallel()
	assertPublishedDoc(t, localtools.NewPrint(nil).(schemaRepo), localtools.PrintToolsName,
		map[string]string{localtools.ToolPrint: "Print"})
}

// printPolicyRecorder captures the pair the HITL wrapper evaluates on, which is
// what a [hitl] rule and a policy block are written against.
type printPolicyRecorder struct {
	action    hitlservice.Action
	toolsName string
	toolName  string
	args      map[string]any
}

func (r *printPolicyRecorder) Evaluate(_ context.Context, toolsName, toolName string, args map[string]any) (hitlservice.EvaluationResult, error) {
	r.toolsName, r.toolName, r.args = toolsName, toolName, args
	return hitlservice.EvaluationResult{Action: r.action}, nil
}

// TestUnit_PrintTools_PolicyArgsAndHITLPlumbing proves the two halves of the
// plumbing a declared native toolset depends on: arguments reach the tool by
// both routes the engine uses, and a [tools_policies.native-print] block reaches
// it through taskengine.ToolsArgsFromContext without ever becoming a tool
// argument — the toolset declares no knobs, so the block must change nothing.
func TestUnit_PrintTools_PolicyArgsAndHITLPlumbing(t *testing.T) {
	t.Parallel()
	repo := localtools.NewPrint(nil)

	// Route one: a model-driven call carries arguments as the input map.
	res, dt, err := execPrint(t, context.Background(), repo,
		map[string]any{"message": "from the model"}, printCall(localtools.ToolPrint, nil))
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeString, dt)
	assert.Equal(t, "from the model", res)

	// Route two: a declarative `tools` task carries them on the ToolsCall, with
	// the chain's current output as input.
	res, dt, err = execPrint(t, context.Background(), repo,
		"the chain's output", printCall("", map[string]string{"message": "from the chain"}))
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeString, dt)
	assert.Equal(t, "from the chain", res)

	// A policy block keyed to this toolset is delivered under the registered
	// name; print consumes no knob, so it must not leak into the output and must
	// not be mistaken for an unknown argument.
	policy := map[string]string{"_unused_knob": "value"}
	ctx := taskengine.WithToolsArgs(context.Background(), localtools.PrintToolsName, policy)
	require.Equal(t, policy, taskengine.ToolsArgsFromContext(ctx, localtools.PrintToolsName),
		"policy args must arrive under the registered toolset name, or a seeded block addresses nothing")
	require.Nil(t, taskengine.ToolsArgsFromContext(ctx, "local_fs"),
		"policy args are keyed by toolset name; another toolset must not see them")

	res, _, err = execPrint(t, ctx, repo, map[string]any{"message": "unchanged"}, printCall(localtools.ToolPrint, nil))
	require.NoError(t, err)
	assert.Equal(t, "unchanged", res, "a policy block changed the output of a toolset that declares no knobs")
}

// TestUnit_PrintTools_GoesThroughTheSharedHITLGate proves the toolset carries no
// gate of its own: it exposes no Prechecker, the call only lands when the shared
// wrapper's policy lets it, and the wrapper evaluates on the registered toolset
// name paired with the tool.
func TestUnit_PrintTools_GoesThroughTheSharedHITLGate(t *testing.T) {
	t.Parallel()

	_, ok := localtools.NewPrint(nil).(taskengine.Prechecker)
	assert.False(t, ok, "this toolset implements Prechecker; approval belongs to the HITL wrapper alone")

	args := map[string]any{"message": "gated"}
	call := printCall(localtools.ToolPrint, nil)

	denyTracker := &recTracker{}
	recorder := &printPolicyRecorder{action: hitlservice.ActionDeny}
	denied := localtools.NewHITLWrapper(localtools.NewPrint(denyTracker), alwaysApprove, recorder, nil)
	res, _, err := denied.Exec(context.Background(), time.Now(), args, false, call)
	require.NoError(t, err)
	assert.Contains(t, res, "Denied by the active policy")
	assert.Zero(t, denyTracker.starts.Load(), "the toolset ran despite a denying policy")
	assert.Equal(t, localtools.PrintToolsName, recorder.toolsName,
		"the wrapper evaluates on the registered toolset name, which is what a rule is written against")
	assert.Equal(t, localtools.ToolPrint, recorder.toolName)
	assert.Equal(t, "gated", recorder.args["message"], "the wrapper cannot show a human what it never saw")

	approveTracker := &recTracker{}
	approved := localtools.NewHITLWrapper(localtools.NewPrint(approveTracker), alwaysApprove, approvePolicy(), nil)
	res, dt, err := approved.Exec(context.Background(), time.Now(), args, false, call)
	require.NoError(t, err)
	assert.Equal(t, taskengine.DataTypeString, dt)
	assert.Equal(t, "gated", res)
	assert.EqualValues(t, 1, approveTracker.starts.Load(), "an approved call must reach the toolset exactly once")
}

func TestUnit_PrintTools_AppendsToChatHistory(t *testing.T) {
	t.Parallel()

	hist := taskengine.ChatHistory{Messages: []taskengine.Message{{Role: "user", Content: "hi"}}}
	res, dt, err := execPrint(t, context.Background(), localtools.NewPrint(nil), hist,
		printCall(localtools.ToolPrint, map[string]string{"message": "noted"}))
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)

	out, ok := res.(taskengine.ChatHistory)
	require.True(t, ok, "a ChatHistory input must come back as a ChatHistory, or the next task cannot read it")
	require.Len(t, out.Messages, 2)
	assert.Equal(t, "user", out.Messages[0].Role)
	assert.Equal(t, "system", out.Messages[1].Role)
	assert.Equal(t, "noted", out.Messages[1].Content)
	assert.False(t, out.Messages[1].Timestamp.IsZero())
}

func TestUnit_PrintTools_ArgumentFailuresAreRecoverable(t *testing.T) {
	t.Parallel()
	repo := localtools.NewPrint(nil)
	ctx := context.Background()

	for name, tc := range map[string]struct {
		input any
		call  *taskengine.ToolsCall
		want  string
	}{
		"missing message": {
			input: map[string]any{},
			call:  printCall(localtools.ToolPrint, nil),
			want:  "missing 'message' argument",
		},
		"unknown argument": {
			input: map[string]any{"message": "hi", "colour": "red"},
			call:  printCall(localtools.ToolPrint, nil),
			want:  "unknown argument(s): colour",
		},
		"unknown tool": {
			input: map[string]any{"message": "hi"},
			call:  printCall("println", nil),
			want:  `unknown tool "println"`,
		},
		"no tools call": {
			input: map[string]any{"message": "hi"},
			call:  nil,
			want:  "tools required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := execPrint(t, ctx, repo, tc.input, tc.call)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "recoverable",
				"a model cannot tell a bad argument from a broken environment without the marker: %v", err)
		})
	}
}

func TestUnit_PrintTools_RendersANonStringArgument(t *testing.T) {
	t.Parallel()

	res, _, err := execPrint(t, context.Background(), localtools.NewPrint(nil),
		map[string]any{"message": 42}, printCall(localtools.ToolPrint, nil))
	require.NoError(t, err)
	assert.Equal(t, "42", res)
}
