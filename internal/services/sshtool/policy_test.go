package sshtool_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/sshtool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withPolicy attaches a chain's [tools_policies.native-ssh] block the way the
// engine does: keyed by the toolset name, as strings.
func withPolicy(args map[string]string) context.Context {
	return taskengine.WithToolsArgs(context.Background(), sshtool.ToolsProviderName, args)
}

func execOn(ctx context.Context, repo taskengine.ToolsRepo, args map[string]any) (any, taskengine.DataType, error) {
	return repo.Exec(ctx, time.Now(), args, false, call(sshtool.ToolsProviderName))
}

func mustExecOn(t *testing.T, ctx context.Context, repo taskengine.ToolsRepo, args map[string]any) *sshtool.SSHResult {
	t.Helper()
	out, dt, err := execOn(ctx, repo, args)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	res, ok := out.(*sshtool.SSHResult)
	require.Truef(t, ok, "result is %T, want *sshtool.SSHResult", out)
	return res
}

// precheck is the boundary as the wrapper sees it: refused from static
// configuration alone, with nothing dialled.
func precheck(t *testing.T, ctx context.Context, repo taskengine.ToolsRepo, args map[string]any) error {
	t.Helper()
	pre, ok := repo.(taskengine.Prechecker)
	require.True(t, ok)
	return pre.Precheck(ctx, args, call(sshtool.ToolsProviderName))
}

func remoteArgs(host, user string, port int) map[string]any {
	return map[string]any{"host": host, "user": user, "port": port, "command": "uptime", "password": "x"}
}

// TestUnit_Policy_AllowedHostsComesFromTheToolsPolicy pins the args plumbing.
// This toolset reaches OTHER machines, so unlike every other native toolset the
// declaration naming it is not consent on its own: the policy block says which
// machine, and nothing else can.
func TestUnit_Policy_AllowedHostsComesFromTheToolsPolicy(t *testing.T) {
	t.Parallel()
	repo := newTools(t, "")

	cases := []struct {
		name    string
		policy  map[string]string
		args    map[string]any
		wantErr error
	}{
		{"no policy reaches no machine", nil, remoteArgs("build.example.com", "deploy", 22), sshtool.ErrNoAllowedHosts},
		{"an empty value reaches no machine", map[string]string{"_allowed_hosts": "   "}, remoteArgs("build.example.com", "deploy", 22), sshtool.ErrNoAllowedHosts},
		{"an unrelated key is not an allowlist", map[string]string{"_timeout": "5s"}, remoteArgs("build.example.com", "deploy", 22), sshtool.ErrNoAllowedHosts},
		{"a named host is reachable", map[string]string{"_allowed_hosts": "build.example.com"}, remoteArgs("build.example.com", "deploy", 22), nil},
		{"a host matches case-insensitively", map[string]string{"_allowed_hosts": "build.example.com"}, remoteArgs("BUILD.Example.COM", "deploy", 22), nil},
		{"an unnamed host is refused", map[string]string{"_allowed_hosts": "build.example.com"}, remoteArgs("prod.example.com", "deploy", 22), sshtool.ErrHostNotAllowed},
		{"one of several named hosts is reachable", map[string]string{"_allowed_hosts": "a.example.com, b.example.com"}, remoteArgs("b.example.com", "deploy", 22), nil},
		{"a domain entry covers a subdomain", map[string]string{"_allowed_hosts": "*.example.com"}, remoteArgs("build.example.com", "deploy", 22), nil},
		{"a domain entry does not cover the apex", map[string]string{"_allowed_hosts": "*.example.com"}, remoteArgs("example.com", "deploy", 22), sshtool.ErrHostNotAllowed},
		{"a domain entry is not a substring match", map[string]string{"_allowed_hosts": "*.example.com"}, remoteArgs("evil-example.com", "deploy", 22), sshtool.ErrHostNotAllowed},
		{"a user-qualified entry binds the account", map[string]string{"_allowed_hosts": "deploy@build.example.com"}, remoteArgs("build.example.com", "deploy", 22), nil},
		{"a user-qualified entry refuses another account", map[string]string{"_allowed_hosts": "deploy@build.example.com"}, remoteArgs("build.example.com", "root", 22), sshtool.ErrHostNotAllowed},
		{"a port-qualified entry binds the port", map[string]string{"_allowed_hosts": "build.example.com:2222"}, remoteArgs("build.example.com", "deploy", 2222), nil},
		{"a port-qualified entry refuses another port", map[string]string{"_allowed_hosts": "build.example.com:2222"}, remoteArgs("build.example.com", "deploy", 22), sshtool.ErrHostNotAllowed},
		{"an unqualified entry accepts any port", map[string]string{"_allowed_hosts": "build.example.com"}, remoteArgs("build.example.com", "deploy", 2222), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := precheck(t, withPolicy(tc.policy), repo, tc.args)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// A wildcard entry would restore the reach-any-machine default the allowlist
// exists to remove, so it is a configuration error rather than a grant.
func TestUnit_Policy_WildcardIsNotAnAllowlist(t *testing.T) {
	t.Parallel()
	repo := newTools(t, "")

	for _, entry := range []string{"*", " * ", "a.example.com,*", "*.*", "ex*.com"} {
		err := precheck(t, withPolicy(map[string]string{"_allowed_hosts": entry}), repo, remoteArgs("a.example.com", "deploy", 22))
		require.Errorf(t, err, "entry %q was accepted as an allowlist", entry)
		assert.Containsf(t, err.Error(), "fatal:", "entry %q is refused as if retrying could fix it", entry)
	}
}

// The refusal has to name the block that would fix it, under this toolset's
// registered name, and say that no argument spelling is the fix.
func TestUnit_Policy_RefusalNamesTheBlockThatWouldFixIt(t *testing.T) {
	t.Parallel()

	err := precheck(t, context.Background(), newTools(t, ""), remoteArgs("build.example.com", "deploy", 22))
	require.Error(t, err)
	assert.ErrorIs(t, err, sshtool.ErrNoAllowedHosts)
	assert.Contains(t, err.Error(), "tools_policies."+sshtool.ToolsProviderName+"._allowed_hosts")
	assert.Contains(t, err.Error(), "fatal:", "no argument spelling reaches a machine nothing named")

	// A host outside a populated list is recoverable, and says what IS reachable.
	err = precheck(t, withPolicy(map[string]string{"_allowed_hosts": "a.example.com"}), newTools(t, ""), remoteArgs("b.example.com", "deploy", 22))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a.example.com", "the refusal does not say which hosts are reachable")
	assert.Contains(t, err.Error(), "recoverable")
}

// A policy attached to another toolset's key must not reach this one; the args
// context is per-toolset and a leak would let one declaration retune another.
func TestUnit_Policy_IsScopedToThisToolsetsName(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"local_fs", "ssh", "native-shell", "decl-reviewer-ssh"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			ctx := taskengine.WithToolsArgs(context.Background(), key, map[string]string{"_allowed_hosts": "build.example.com"})
			err := precheck(t, ctx, newTools(t, ""), remoteArgs("build.example.com", "deploy", 22))
			require.Errorf(t, err, "%q's policy block reached this toolset", key)
			assert.ErrorIs(t, err, sshtool.ErrNoAllowedHosts)
		})
	}
}

// The operator's WithAllowedHosts is a ceiling, not a default: a declaration can
// only narrow within it, so a chain cannot name a machine the runtime never
// offered.
func TestUnit_Policy_OperatorCeilingIntersectsTheDeclaration(t *testing.T) {
	t.Parallel()
	repo := newTools(t, "", sshtool.WithAllowedHosts("a.example.com", "b.example.com"))

	t.Run("the ceiling alone is consent", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, precheck(t, context.Background(), repo, remoteArgs("a.example.com", "deploy", 22)))
	})

	t.Run("a host outside the ceiling is refused with no policy", func(t *testing.T) {
		t.Parallel()
		err := precheck(t, context.Background(), repo, remoteArgs("c.example.com", "deploy", 22))
		require.Error(t, err)
		assert.ErrorIs(t, err, sshtool.ErrHostNotAllowed)
	})

	t.Run("a declaration narrows within the ceiling", func(t *testing.T) {
		t.Parallel()
		ctx := withPolicy(map[string]string{"_allowed_hosts": "b.example.com"})
		require.NoError(t, precheck(t, ctx, repo, remoteArgs("b.example.com", "deploy", 22)))
		err := precheck(t, ctx, repo, remoteArgs("a.example.com", "deploy", 22))
		require.Error(t, err, "the declaration's narrower list did not bind")
		assert.ErrorIs(t, err, sshtool.ErrHostNotAllowed)
	})

	t.Run("a declaration cannot widen past the ceiling", func(t *testing.T) {
		t.Parallel()
		ctx := withPolicy(map[string]string{"_allowed_hosts": "c.example.com"})
		err := precheck(t, ctx, repo, remoteArgs("c.example.com", "deploy", 22))
		require.Error(t, err, "a chain named a host the runtime never offered")
		assert.ErrorIs(t, err, sshtool.ErrHostNotAllowed)
		assert.Contains(t, err.Error(), "runtime's SSH host allowlist")
	})
}

// The chain's static tools args are authored by a human and outrank the model's,
// so a declaration that pins the host cannot be redirected by the call.
func TestUnit_Policy_StaticToolsArgsOutrankTheModel(t *testing.T) {
	t.Parallel()
	repo := newTools(t, "")
	pre, ok := repo.(taskengine.Prechecker)
	require.True(t, ok)

	pinned := &taskengine.ToolsCall{
		Name:     sshtool.ToolsProviderName,
		ToolName: sshtool.ToolExecuteRemoteCommand,
		Args:     map[string]string{"host": "build.example.com", "user": "deploy"},
	}
	ctx := withPolicy(map[string]string{"_allowed_hosts": "build.example.com"})

	require.NoError(t, pre.Precheck(ctx, map[string]any{"command": "uptime", "password": "x"}, pinned))
	require.NoError(t, pre.Precheck(ctx, map[string]any{
		"host": "prod.example.com", "user": "root", "command": "uptime", "password": "x",
	}, pinned), "the chain's pinned host did not win over the model's")
}

type fakePolicy struct {
	action   hitlservice.Action
	err      error
	calls    int
	gotTools string
	gotTool  string
	gotHost  any
	gotCmd   any
}

func (f *fakePolicy) Evaluate(_ context.Context, toolsName, toolName string, args map[string]any) (hitlservice.EvaluationResult, error) {
	f.calls++
	f.gotTools, f.gotTool = toolsName, toolName
	f.gotHost, f.gotCmd = args["host"], args["command"]
	if f.err != nil {
		return hitlservice.EvaluationResult{}, f.err
	}
	return hitlservice.EvaluationResult{Action: f.action, PolicyName: "test-policy"}, nil
}

func ask(approve bool) localtools.AskApproval {
	return func(context.Context, hitlservice.ApprovalRequest) (bool, error) { return approve, nil }
}

// TestUnit_HITL_EveryCallPassesTheWrapper is the second half of the declaration
// bargain: naming the toolset and enumerating a host grants reach, it does not
// grant unattended execution. The gate is the wrapper the engine already puts
// around PersistentRepo, and this toolset must be gateable by it unchanged.
func TestUnit_HITL_EveryCallPassesTheWrapper(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)
	ctx := withPolicy(map[string]string{"_allowed_hosts": srv.host})
	args := srv.callArgs("greet")

	t.Run("allow runs the command", func(t *testing.T) {
		pol := &fakePolicy{action: hitlservice.ActionAllow}
		wrapped := localtools.NewHITLWrapper(newTools(t, srv.knownHosts(t)), ask(false), pol, nil)
		res := mustExecOn(t, ctx, wrapped, args)
		assert.True(t, res.Success)
		assert.Equal(t, "hello from the remote", res.Stdout)

		// The policy keys on the registered toolset name and the unprefixed
		// tool, and it sees the call's own arguments.
		assert.Equal(t, 1, pol.calls)
		assert.Equal(t, sshtool.ToolsProviderName, pol.gotTools)
		assert.Equal(t, sshtool.ToolExecuteRemoteCommand, pol.gotTool)
		assert.Equal(t, srv.host, pol.gotHost)
		assert.Equal(t, "greet", pol.gotCmd)
	})

	t.Run("deny never reaches the machine", func(t *testing.T) {
		before := len(srv.commands())
		pol := &fakePolicy{action: hitlservice.ActionDeny}
		wrapped := localtools.NewHITLWrapper(newTools(t, srv.knownHosts(t)), ask(true), pol, nil)
		out, dt, err := execOn(ctx, wrapped, args)
		require.NoError(t, err)
		assert.Equal(t, taskengine.DataTypeString, dt)
		assert.NotContains(t, out, "hello from the remote")
		assert.Len(t, srv.commands(), before, "a denied call still ran a command on the remote host")
	})

	t.Run("approve asks, and a refusal never reaches the machine", func(t *testing.T) {
		before := len(srv.commands())
		pol := &fakePolicy{action: hitlservice.ActionApprove}
		wrapped := localtools.NewHITLWrapper(newTools(t, srv.knownHosts(t)), ask(false), pol, nil)
		out, dt, err := execOn(ctx, wrapped, args)
		require.NoError(t, err)
		assert.Equal(t, taskengine.DataTypeString, dt)
		assert.NotContains(t, out, "hello from the remote")
		assert.Len(t, srv.commands(), before, "a refused approval still ran a command on the remote host")
	})

	t.Run("approve runs the command once granted", func(t *testing.T) {
		pol := &fakePolicy{action: hitlservice.ActionApprove}
		wrapped := localtools.NewHITLWrapper(newTools(t, srv.knownHosts(t)), ask(true), pol, nil)
		res := mustExecOn(t, ctx, wrapped, args)
		assert.True(t, res.Success)
	})

	t.Run("a policy error denies rather than falling through", func(t *testing.T) {
		before := len(srv.commands())
		pol := &fakePolicy{err: errors.New("policy source unavailable")}
		wrapped := localtools.NewHITLWrapper(newTools(t, srv.knownHosts(t)), ask(true), pol, nil)
		out, dt, err := execOn(ctx, wrapped, args)
		require.NoError(t, err)
		assert.Equal(t, taskengine.DataTypeString, dt)
		assert.NotContains(t, out, "hello from the remote")
		assert.Len(t, srv.commands(), before)
	})
}

// TestUnit_HITL_UnallowedHostIsRefusedBeforeAnyoneIsAsked is why the boundary is
// a Precheck and not an Exec-time check: a host nothing declared must not cost a
// human an approval decision, and must not open a connection to find out.
func TestUnit_HITL_UnallowedHostIsRefusedBeforeAnyoneIsAsked(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)
	before := len(srv.commands())

	asked := false
	pol := &fakePolicy{action: hitlservice.ActionApprove}
	wrapped := localtools.NewHITLWrapper(newTools(t, srv.knownHosts(t)),
		func(context.Context, hitlservice.ApprovalRequest) (bool, error) {
			asked = true
			return true, nil
		}, pol, nil)

	ctx := withPolicy(map[string]string{"_allowed_hosts": "somewhere.else.example.com"})
	_, _, err := execOn(ctx, wrapped, srv.callArgs("greet"))
	require.Error(t, err)
	assert.ErrorIs(t, err, sshtool.ErrHostNotAllowed)
	assert.False(t, asked, "a host outside the allowlist cost a human an approval decision")
	assert.Len(t, srv.commands(), before, "a host outside the allowlist was connected to anyway")
}

// The wrapper reads the tools policy off the same context it gates on, so the
// two plumbings compose: a gated call still resolves its reach from the chain's
// _allowed_hosts.
func TestUnit_HITL_ToolsPolicyStillAppliesUnderTheGate(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)
	pol := &fakePolicy{action: hitlservice.ActionAllow}
	wrapped := localtools.NewHITLWrapper(newTools(t, srv.knownHosts(t)), ask(true), pol, nil)

	res := mustExecOn(t, withPolicy(map[string]string{"_allowed_hosts": srv.host}), wrapped, srv.callArgs("greet"))
	assert.True(t, res.Success)

	_, _, err := execOn(withPolicy(nil), wrapped, srv.callArgs("greet"))
	require.Error(t, err, "the gate suppressed the policy that scopes the toolset")
	assert.ErrorIs(t, err, sshtool.ErrNoAllowedHosts)
}
