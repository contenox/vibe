package jqtool_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/jqtool"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withPolicy attaches a chain's [tools_policies.native-jq] block the way the
// engine does: keyed by the toolset name, as strings.
func withPolicy(args map[string]string) context.Context {
	return taskengine.WithToolsArgs(context.Background(), jqtool.ToolsProviderName, args)
}

func execOn(ctx context.Context, repo taskengine.ToolsRepo, args map[string]any) (any, taskengine.DataType, error) {
	return repo.Exec(ctx, time.Now(), args, false,
		&taskengine.ToolsCall{Name: jqtool.ToolsProviderName, ToolName: jqtool.ToolQuery})
}

func mustExecOn(t *testing.T, ctx context.Context, repo taskengine.ToolsRepo, args map[string]any) *jqtool.Result {
	t.Helper()
	out, dt, err := execOn(ctx, repo, args)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	res, ok := out.(*jqtool.Result)
	require.Truef(t, ok, "result is %T, want *jqtool.Result", out)
	return res
}

// TestUnit_Policy_AllowedDirComesFromTheToolsPolicy pins the args plumbing: the
// directory `path` resolves against is the chain's when it says so, and the
// constructor's otherwise.
func TestUnit_Policy_AllowedDirComesFromTheToolsPolicy(t *testing.T) {
	t.Parallel()
	ctor := t.TempDir()
	policy := t.TempDir()
	writeFixture(t, ctor, "chain.json", `{"who":"constructor"}`)
	writeFixture(t, policy, "chain.json", `{"who":"policy"}`)
	writeFixture(t, filepath.Join(policy, "nested"), "chain.json", `{"who":"nested"}`)

	cases := []struct {
		name   string
		policy map[string]string
		want   string
	}{
		{"no policy keeps the constructor's dir", nil, `"constructor"`},
		{"an absolute policy dir binds", map[string]string{"_allowed_dir": policy}, `"policy"`},
		{"an empty value falls back to the constructor", map[string]string{"_allowed_dir": ""}, `"constructor"`},
		{"whitespace falls back to the constructor", map[string]string{"_allowed_dir": "   "}, `"constructor"`},
		{"an unrelated key is ignored", map[string]string{"_max_read_bytes": "10"}, `"constructor"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := mustExecOn(t, withPolicy(tc.policy), jqtool.NewTools(ctor), map[string]any{"path": "chain.json", "filter": ".who"})
			assert.Equal(t, []string{tc.want}, values(res))
		})
	}

	t.Run("a relative policy dir is anchored on the session cwd", func(t *testing.T) {
		t.Parallel()
		ctx := vfs.WithSessionCwd(withPolicy(map[string]string{"_allowed_dir": "nested"}), policy)
		res := mustExecOn(t, ctx, jqtool.NewTools(ctor), map[string]any{"path": "chain.json", "filter": ".who"})
		assert.Equal(t, []string{`"nested"`}, values(res),
			"a relative _allowed_dir must resolve against the session's workspace root, not the process cwd")
	})

	t.Run("a relative policy dir with no root is fatal and names the key", func(t *testing.T) {
		t.Parallel()
		_, _, err := execOn(withPolicy(map[string]string{"_allowed_dir": "nested"}), jqtool.NewTools(""),
			map[string]any{"path": "chain.json", "filter": "."})
		require.Error(t, err)
		require.ErrorIs(t, err, jqtool.ErrNoWorkspaceRoot)
		assert.Contains(t, err.Error(), "tools_policies."+jqtool.ToolsProviderName+"._allowed_dir",
			"the refusal must name the policy block that set it, under this toolset's registered name")
		assert.Contains(t, err.Error(), "fatal:", "no path spelling fixes an unanchorable root")
	})

	t.Run("a policy dir still contains the tool", func(t *testing.T) {
		t.Parallel()
		_, _, err := execOn(withPolicy(map[string]string{"_allowed_dir": filepath.Join(policy, "nested")}),
			jqtool.NewTools(ctor), map[string]any{"path": "../chain.json", "filter": "."})
		require.Error(t, err, "the policy narrows the boundary; it does not remove it")
		assert.ErrorIs(t, err, jqtool.ErrEscapesWorkspace)
	})
}

// The session cwd resolver the ACP profile supplies is the other half of the
// same plumbing: with no declared boundary the toolset is scoped per call.
func TestUnit_Policy_SessionCwdResolverScopesTheToolset(t *testing.T) {
	t.Parallel()
	session := t.TempDir()
	writeFixture(t, session, "chain.json", `{"who":"session"}`)

	repo := jqtool.NewToolsWith("", jqtool.ToolsProviderName, func(context.Context) string { return session })
	res := mustExecOn(t, context.Background(), repo, map[string]any{"path": "chain.json", "filter": ".who"})
	assert.Equal(t, []string{`"session"`}, values(res))

	// The context's cwd wins over the constructor's resolver, so a resumed
	// session is scoped by the session and not by the process that revived it.
	other := t.TempDir()
	writeFixture(t, other, "chain.json", `{"who":"context"}`)
	res = mustExecOn(t, vfs.WithSessionCwd(context.Background(), other), repo, map[string]any{"path": "chain.json", "filter": ".who"})
	assert.Equal(t, []string{`"context"`}, values(res))
}

// A policy attached to another toolset's key must not reach this one; the args
// context is per-toolset and a leak would let one declaration retune another.
func TestUnit_Policy_IsScopedToThisToolsetsName(t *testing.T) {
	t.Parallel()
	ctor := t.TempDir()
	other := t.TempDir()
	writeFixture(t, ctor, "chain.json", `{"who":"constructor"}`)
	writeFixture(t, other, "chain.json", `{"who":"elsewhere"}`)

	for _, key := range []string{"local_fs", "jq", "decl-reviewer-jq"} {
		t.Run(key, func(t *testing.T) {
			ctx := taskengine.WithToolsArgs(context.Background(), key, map[string]string{"_allowed_dir": other})
			res := mustExecOn(t, ctx, jqtool.NewTools(ctor), map[string]any{"path": "chain.json", "filter": ".who"})
			assert.Equalf(t, []string{`"constructor"`}, values(res), "%q's policy block reached this toolset", key)
		})
	}
}

// The policy scopes the tool; it is not a second approval gate. A refused call
// is refused by the wrapper above, so nothing here may turn a policy value into
// a denial.
func TestUnit_Policy_NeverRefusesACall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "chain.json", `{"who":"constructor"}`)

	for _, args := range []map[string]string{
		{"_allowed_dir": ""},
		{"_denied": "everything"},
		{"_allowed_dir": dir, "_max_read_bytes": "0"},
	} {
		res := mustExecOn(t, withPolicy(args), jqtool.NewTools(dir), map[string]any{"path": "chain.json", "filter": ".who"})
		assert.Equalf(t, []string{`"constructor"`}, values(res), "policy %v suppressed the query", args)
	}
}

type fakePolicy struct {
	action    hitlservice.Action
	err       error
	calls     int
	gotTools  string
	gotTool   string
	gotFilter any
	gotPath   any
}

func (f *fakePolicy) Evaluate(_ context.Context, toolsName, toolName string, args map[string]any) (hitlservice.EvaluationResult, error) {
	f.calls++
	f.gotTools, f.gotTool = toolsName, toolName
	f.gotFilter, f.gotPath = args["filter"], args["path"]
	if f.err != nil {
		return hitlservice.EvaluationResult{}, f.err
	}
	return hitlservice.EvaluationResult{Action: f.action, PolicyName: "test-policy"}, nil
}

// TestUnit_HITL_EveryCallPassesTheWrapper is the second half of the declaration
// bargain: naming the toolset grants machine-local reach, it does not grant
// unattended execution. The gate is the wrapper the engine already puts around
// PersistentRepo, and this toolset must be gateable by it unchanged.
func TestUnit_HITL_EveryCallPassesTheWrapper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, dir, "chain.json", `{"who":"constructor"}`)
	args := map[string]any{"path": "chain.json", "filter": ".who"}

	ask := func(approve bool) localtools.AskApproval {
		return func(context.Context, hitlservice.ApprovalRequest) (bool, error) { return approve, nil }
	}

	t.Run("allow runs the query", func(t *testing.T) {
		t.Parallel()
		pol := &fakePolicy{action: hitlservice.ActionAllow}
		wrapped := localtools.NewHITLWrapper(jqtool.NewTools(dir), ask(false), pol, nil)
		res := mustExecOn(t, context.Background(), wrapped, args)
		assert.Equal(t, []string{`"constructor"`}, values(res))

		// The policy keys on the registered toolset name and the unprefixed
		// tool, and it sees the call's own arguments.
		assert.Equal(t, 1, pol.calls)
		assert.Equal(t, jqtool.ToolsProviderName, pol.gotTools)
		assert.Equal(t, jqtool.ToolQuery, pol.gotTool)
		assert.Equal(t, ".who", pol.gotFilter)
		assert.Equal(t, "chain.json", pol.gotPath)
	})

	t.Run("deny never reaches the document", func(t *testing.T) {
		t.Parallel()
		pol := &fakePolicy{action: hitlservice.ActionDeny}
		wrapped := localtools.NewHITLWrapper(jqtool.NewTools(dir), ask(true), pol, nil)
		out, dt, err := execOn(context.Background(), wrapped, args)
		require.NoError(t, err)
		assert.Equal(t, taskengine.DataTypeString, dt)
		assert.NotContains(t, out, "constructor", "a denied call must not carry the document it was denied")
	})

	t.Run("approve asks, and a refusal stops the query", func(t *testing.T) {
		t.Parallel()
		pol := &fakePolicy{action: hitlservice.ActionApprove}
		wrapped := localtools.NewHITLWrapper(jqtool.NewTools(dir), ask(false), pol, nil)
		out, dt, err := execOn(context.Background(), wrapped, args)
		require.NoError(t, err)
		assert.Equal(t, taskengine.DataTypeString, dt)
		assert.NotContains(t, out, "constructor")
	})

	t.Run("approve runs the query once granted", func(t *testing.T) {
		t.Parallel()
		pol := &fakePolicy{action: hitlservice.ActionApprove}
		wrapped := localtools.NewHITLWrapper(jqtool.NewTools(dir), ask(true), pol, nil)
		res := mustExecOn(t, context.Background(), wrapped, args)
		assert.Equal(t, []string{`"constructor"`}, values(res))
	})

	t.Run("a policy error denies rather than falling through", func(t *testing.T) {
		t.Parallel()
		pol := &fakePolicy{err: errors.New("policy source unavailable")}
		wrapped := localtools.NewHITLWrapper(jqtool.NewTools(dir), ask(true), pol, nil)
		out, dt, err := execOn(context.Background(), wrapped, args)
		require.NoError(t, err)
		assert.Equal(t, taskengine.DataTypeString, dt)
		assert.NotContains(t, out, "constructor")
	})
}

// The wrapper reads the tools policy off the same context it gates on, so the
// two plumbings compose: a gated call still resolves its path under the chain's
// _allowed_dir.
func TestUnit_HITL_ToolsPolicyStillAppliesUnderTheGate(t *testing.T) {
	t.Parallel()
	ctor := t.TempDir()
	policy := t.TempDir()
	writeFixture(t, ctor, "chain.json", `{"who":"constructor"}`)
	writeFixture(t, policy, "chain.json", `{"who":"policy"}`)

	pol := &fakePolicy{action: hitlservice.ActionAllow}
	wrapped := localtools.NewHITLWrapper(jqtool.NewTools(ctor),
		func(context.Context, hitlservice.ApprovalRequest) (bool, error) { return true, nil }, pol, nil)

	res := mustExecOn(t, withPolicy(map[string]string{"_allowed_dir": policy}), wrapped,
		map[string]any{"path": "chain.json", "filter": ".who"})
	assert.Equal(t, []string{`"policy"`}, values(res))
}
