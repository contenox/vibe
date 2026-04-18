package hitlservice

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/vfsservice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nopKVReader is a KVReader that always returns ErrNotFound, causing the service
// to fall back to the default policy file name.
type nopKVReader struct{}

func (nopKVReader) GetKV(_ context.Context, _ string, _ any) error {
	return errors.New("not found")
}

// fixedKVReader returns a specific policy name for any key lookup.
type fixedKVReader struct{ name string }

func (f fixedKVReader) GetKV(_ context.Context, _ string, out any) error {
	if p, ok := out.(*string); ok {
		*p = f.name
	}
	return nil
}

// ── evaluate ─────────────────────────────────────────────────────────────────

func TestEvaluate_FirstMatchWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		policy     *Policy
		hook       string
		tool       string
		args       map[string]any
		wantAction Action
		wantReason string
	}{
		{
			name:   "exact match approve",
			policy: &Policy{Rules: []Rule{{Hook: "local_fs", Tool: "write_file", Action: ActionApprove}}},
			hook:   "local_fs", tool: "write_file",
			wantAction: ActionApprove, wantReason: ReasonMatchedRule,
		},
		{
			name:   "exact match deny",
			policy: &Policy{Rules: []Rule{{Hook: "local_fs", Tool: "write_file", Action: ActionDeny}}},
			hook:   "local_fs", tool: "write_file",
			wantAction: ActionDeny, wantReason: ReasonMatchedRule,
		},
		{
			name:   "wildcard hook",
			policy: &Policy{Rules: []Rule{{Hook: "*", Tool: "write_file", Action: ActionApprove}}},
			hook:   "anything", tool: "write_file",
			wantAction: ActionApprove, wantReason: ReasonMatchedRule,
		},
		{
			name:   "wildcard tool",
			policy: &Policy{Rules: []Rule{{Hook: "local_fs", Tool: "*", Action: ActionApprove}}},
			hook:   "local_fs", tool: "anything",
			wantAction: ActionApprove, wantReason: ReasonMatchedRule,
		},
		{
			name:   "empty hook matches all",
			policy: &Policy{Rules: []Rule{{Hook: "", Tool: "write_file", Action: ActionApprove}}},
			hook:   "local_fs", tool: "write_file",
			wantAction: ActionApprove, wantReason: ReasonMatchedRule,
		},
		{
			name:   "no match defaults to allow when default_action absent",
			policy: &Policy{Rules: []Rule{{Hook: "local_fs", Tool: "write_file", Action: ActionApprove}}},
			hook:   "webhook", tool: "call",
			wantAction: ActionAllow, wantReason: ReasonDefaultAction,
		},
		{
			name: "first rule wins",
			policy: &Policy{Rules: []Rule{
				{Hook: "local_fs", Tool: "write_file", Action: ActionDeny},
				{Hook: "local_fs", Tool: "write_file", Action: ActionApprove},
			}},
			hook: "local_fs", tool: "write_file",
			wantAction: ActionDeny, wantReason: ReasonMatchedRule,
		},
		{
			name:   "empty rules — default allow",
			policy: &Policy{Rules: []Rule{}},
			hook:   "local_fs", tool: "write_file",
			wantAction: ActionAllow, wantReason: ReasonDefaultAction,
		},
		{
			name:   "wildcard both matches anything",
			policy: &Policy{Rules: []Rule{{Hook: "*", Tool: "*", Action: ActionApprove}}},
			hook:   "echo", tool: "echo",
			wantAction: ActionApprove, wantReason: ReasonMatchedRule,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := evaluate(tc.policy, tc.hook, tc.tool, tc.args)
			assert.Equal(t, tc.wantAction, got.Action)
			assert.Equal(t, tc.wantReason, got.Reason)
		})
	}
}

func TestEvaluate_DefaultAction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		defaultAction Action
		wantAction    Action
	}{
		{"deny when no rule matches", ActionDeny, ActionDeny},
		{"approve when no rule matches", ActionApprove, ActionApprove},
		{"allow explicit", ActionAllow, ActionAllow},
		{"empty string → allow", "", ActionAllow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &Policy{DefaultAction: tc.defaultAction, Rules: []Rule{}}
			got := evaluate(p, "any", "tool", nil)
			assert.Equal(t, tc.wantAction, got.Action)
			assert.Equal(t, ReasonDefaultAction, got.Reason)
			assert.Nil(t, got.MatchedRule)
		})
	}
}

func TestEvaluate_MatchedRuleIndex(t *testing.T) {
	t.Parallel()
	p := &Policy{Rules: []Rule{
		{Hook: "echo", Tool: "echo", Action: ActionAllow},
		{Hook: "local_fs", Tool: "write_file", Action: ActionApprove},
	}}
	got := evaluate(p, "local_fs", "write_file", nil)
	require.NotNil(t, got.MatchedRule)
	assert.Equal(t, 1, *got.MatchedRule)
}

// ── When conditions ───────────────────────────────────────────────────────────

func TestEvaluate_WhenCondition_GlobMatch(t *testing.T) {
	t.Parallel()
	p := &Policy{
		DefaultAction: ActionDeny,
		Rules: []Rule{
			{
				Hook:   "local_fs",
				Tool:   "write_file",
				When:   []Condition{{Key: "path", Op: OpGlob, Value: "./src/**"}},
				Action: ActionAllow,
			},
		},
	}
	// path inside src/ → allow
	got := evaluate(p, "local_fs", "write_file", map[string]any{"path": "./src/main.go"})
	assert.Equal(t, ActionAllow, got.Action)

	// path outside src/ → default deny
	got = evaluate(p, "local_fs", "write_file", map[string]any{"path": "./etc/passwd"})
	assert.Equal(t, ActionDeny, got.Action)
}

func TestEvaluate_WhenCondition_EqMatch(t *testing.T) {
	t.Parallel()
	p := &Policy{
		Rules: []Rule{
			{
				Hook:   "local_fs",
				Tool:   "read_file",
				When:   []Condition{{Key: "path", Op: OpEq, Value: "secrets.txt"}},
				Action: ActionDeny,
			},
			{Hook: "local_fs", Tool: "read_file", Action: ActionAllow},
		},
	}
	got := evaluate(p, "local_fs", "read_file", map[string]any{"path": "secrets.txt"})
	assert.Equal(t, ActionDeny, got.Action)

	got = evaluate(p, "local_fs", "read_file", map[string]any{"path": "README.md"})
	assert.Equal(t, ActionAllow, got.Action)
}

func TestEvaluate_WhenCondition_MissingKey_RuleSkipped(t *testing.T) {
	t.Parallel()
	p := &Policy{
		DefaultAction: ActionDeny,
		Rules: []Rule{
			{
				Hook:   "local_fs",
				Tool:   "write_file",
				When:   []Condition{{Key: "path", Op: OpGlob, Value: "./src/**"}},
				Action: ActionAllow,
			},
		},
	}
	// args don't contain "path" → condition fails → rule skipped → default deny
	got := evaluate(p, "local_fs", "write_file", map[string]any{"content": "hello"})
	assert.Equal(t, ActionDeny, got.Action)
}

func TestEvaluate_WhenCondition_AllMustMatch(t *testing.T) {
	t.Parallel()
	p := &Policy{
		DefaultAction: ActionApprove,
		Rules: []Rule{
			{
				Hook: "local_fs",
				Tool: "write_file",
				When: []Condition{
					{Key: "path", Op: OpGlob, Value: "./src/**"},
					{Key: "mode", Op: OpEq, Value: "safe"},
				},
				Action: ActionAllow,
			},
		},
	}
	// both conditions match
	got := evaluate(p, "local_fs", "write_file", map[string]any{"path": "./src/a.go", "mode": "safe"})
	assert.Equal(t, ActionAllow, got.Action)

	// one condition fails → rule skipped → default approve
	got = evaluate(p, "local_fs", "write_file", map[string]any{"path": "./src/a.go", "mode": "danger"})
	assert.Equal(t, ActionApprove, got.Action)
}

// ── globMatch ─────────────────────────────────────────────────────────────────

func TestGlobMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		s       string
		want    bool
	}{
		// single star (within component)
		{"*.go", "main.go", true},
		{"*.go", "main.ts", false},
		{"*.go", "sub/main.go", false},

		// double star (across components)
		{"src/**", "src/main.go", true},
		{"src/**", "src/sub/deep/file.go", true},
		{"src/**", "src", true}, // ** matches zero components
		{"src/**", "etc/main.go", false},

		// leading ./ normalized away
		{"./src/**", "./src/main.go", true},
		{"./src/**", "./etc/main.go", false},

		// ** in middle
		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/sub/main.go", true},
		{"src/**/*.go", "src/sub/main.ts", false},

		// ** prefix
		{"**/*.go", "main.go", true},
		{"**/*.go", "src/main.go", true},
		{"**/*.go", "src/sub/main.go", true},
		{"**/*.go", "src/main.ts", false},

		// path traversal bypass prevention
		{"src/**", "../etc/passwd", false},
		{"./src/**", "./src/../etc/passwd", false}, // Clean resolves to etc/passwd

		// exact match (no wildcards)
		{"README.md", "README.md", true},
		{"README.md", "readme.md", false},

		// question mark
		{"file?.go", "file1.go", true},
		{"file?.go", "file10.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"_"+tc.s, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, globMatch(tc.pattern, tc.s))
		})
	}
}

// ── validatePolicy ────────────────────────────────────────────────────────────

func TestValidatePolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		policy  Policy
		wantErr bool
	}{
		{
			name:    "valid — empty rules",
			policy:  Policy{},
			wantErr: false,
		},
		{
			name:    "valid — default deny",
			policy:  Policy{DefaultAction: ActionDeny, Rules: []Rule{{Hook: "local_fs", Tool: "read_file", Action: ActionAllow}}},
			wantErr: false,
		},
		{
			name:    "invalid — unknown default_action",
			policy:  Policy{DefaultAction: "unknown"},
			wantErr: true,
		},
		{
			name:    "invalid — on_timeout allow",
			policy:  Policy{Rules: []Rule{{Hook: "*", Tool: "*", Action: ActionApprove, OnTimeout: ActionAllow}}},
			wantErr: true,
		},
		{
			name:    "valid — on_timeout deny",
			policy:  Policy{Rules: []Rule{{Hook: "*", Tool: "*", Action: ActionApprove, TimeoutS: 60, OnTimeout: ActionDeny}}},
			wantErr: false,
		},
		{
			name:    "invalid — unknown op",
			policy:  Policy{Rules: []Rule{{Hook: "local_fs", Tool: "write_file", Action: ActionAllow, When: []Condition{{Key: "path", Op: "regex", Value: ".*"}}}}},
			wantErr: true,
		},
		{
			name:    "invalid — unknown action",
			policy:  Policy{Rules: []Rule{{Hook: "local_fs", Tool: "write_file", Action: "jump"}}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validatePolicy(&tc.policy)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ── loadPolicy ────────────────────────────────────────────────────────────────

func TestLoadPolicy_ParsesValidJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	ctx := context.Background()
	data := []byte(`{
		"default_action": "deny",
		"rules": [{"hook":"local_fs","tool":"write_file","action":"approve","timeout_s":60,"on_timeout":"deny"}]
	}`)
	_, err := vfs.CreateFile(ctx, &vfsservice.File{Name: "hitl-policy.json", Data: data})
	require.NoError(t, err)

	p, err := loadPolicy(ctx, vfs, "hitl-policy.json")
	require.NoError(t, err)
	assert.Equal(t, ActionDeny, p.DefaultAction)
	require.Len(t, p.Rules, 1)
	assert.Equal(t, ActionApprove, p.Rules[0].Action)
	assert.Equal(t, 60, p.Rules[0].TimeoutS)
	assert.Equal(t, ActionDeny, p.Rules[0].OnTimeout)
}

func TestLoadPolicy_WithWhenCondition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	ctx := context.Background()
	data := []byte(`{"rules":[{"hook":"local_fs","tool":"write_file","when":[{"key":"path","op":"glob","value":"./src/**"}],"action":"allow"}]}`)
	_, err := vfs.CreateFile(ctx, &vfsservice.File{Name: "hitl-policy.json", Data: data})
	require.NoError(t, err)

	p, err := loadPolicy(ctx, vfs, "hitl-policy.json")
	require.NoError(t, err)
	require.Len(t, p.Rules[0].When, 1)
	assert.Equal(t, OpGlob, p.Rules[0].When[0].Op)
	assert.Equal(t, "./src/**", p.Rules[0].When[0].Value)
}

func TestLoadPolicy_RejectsInvalidPolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	ctx := context.Background()
	data := []byte(`{"rules":[{"hook":"*","tool":"*","action":"approve","on_timeout":"allow"}]}`)
	_, err := vfs.CreateFile(ctx, &vfsservice.File{Name: "hitl-policy.json", Data: data})
	require.NoError(t, err)

	_, err = loadPolicy(ctx, vfs, "hitl-policy.json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "on_timeout")
}

func TestLoadPolicy_EmptyRules(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	ctx := context.Background()
	_, err := vfs.CreateFile(ctx, &vfsservice.File{Name: "hitl-policy.json", Data: []byte(`{"rules":[]}`)})
	require.NoError(t, err)

	p, err := loadPolicy(ctx, vfs, "hitl-policy.json")
	require.NoError(t, err)
	assert.Empty(t, p.Rules)
}

func TestLoadPolicy_MissingFile_ReturnsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	_, err := loadPolicy(context.Background(), vfs, "missing.json")
	require.Error(t, err)
}

func TestLoadPolicy_InvalidJSON_ReturnsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	ctx := context.Background()
	_, err := vfs.CreateFile(ctx, &vfsservice.File{Name: "hitl-policy.json", Data: []byte("not json {{{")})
	require.NoError(t, err)
	_, err = loadPolicy(ctx, vfs, "hitl-policy.json")
	require.Error(t, err)
}

// ── defaultPolicy / legacy behaviour ─────────────────────────────────────────

func TestDefaultPolicy_MatchesLegacyBehaviour(t *testing.T) {
	t.Parallel()
	p := defaultPolicy()
	assert.Equal(t, ActionApprove, evaluate(p, "local_fs", "write_file", nil).Action)
	assert.Equal(t, ActionApprove, evaluate(p, "local_fs", "sed", nil).Action)
	assert.Equal(t, ActionApprove, evaluate(p, "local_shell", "local_shell", nil).Action)
	// Tools not in the default policy pass through.
	assert.Equal(t, ActionAllow, evaluate(p, "webhook", "call", nil).Action)
	assert.Equal(t, ActionAllow, evaluate(p, "echo", "echo", nil).Action)
}

// ── Service ───────────────────────────────────────────────────────────────────

func TestService_Evaluate_FallsBackToDefaultWhenFileMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	svc := New(vfs, nopKVReader{}, libtracker.NoopTracker{})
	result, err := svc.Evaluate(context.Background(), "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, ActionApprove, result.Action)
}

func TestService_Evaluate_LoadsFromVFS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	ctx := context.Background()
	data := []byte(`{"default_action":"deny","rules":[{"hook":"webhook","tool":"call","action":"allow"}]}`)
	_, err := vfs.CreateFile(ctx, &vfsservice.File{Name: "hitl-policy.json", Data: data})
	require.NoError(t, err)

	svc := New(vfs, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})
	result, err := svc.Evaluate(ctx, "webhook", "call", nil)
	require.NoError(t, err)
	assert.Equal(t, ActionAllow, result.Action)

	// Unmatched → default deny
	result, err = svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, ActionDeny, result.Action)
}

func TestService_Evaluate_WhenConditionFromVFS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	ctx := context.Background()
	data := []byte(`{
		"default_action": "approve",
		"rules": [
			{"hook":"local_fs","tool":"write_file","when":[{"key":"path","op":"glob","value":"./src/**"}],"action":"allow"},
			{"hook":"local_fs","tool":"write_file","action":"approve","timeout_s":30,"on_timeout":"deny"}
		]
	}`)
	_, err := vfs.CreateFile(ctx, &vfsservice.File{Name: "hitl-policy.json", Data: data})
	require.NoError(t, err)

	svc := New(vfs, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})

	// Scoped write → allow
	result, err := svc.Evaluate(ctx, "local_fs", "write_file", map[string]any{"path": "./src/main.go"})
	require.NoError(t, err)
	assert.Equal(t, ActionAllow, result.Action)
	assert.Equal(t, 0, result.TimeoutS) // no timeout on allow rules

	// Out-of-scope write → fallback approve with timeout
	result, err = svc.Evaluate(ctx, "local_fs", "write_file", map[string]any{"path": "./etc/passwd"})
	require.NoError(t, err)
	assert.Equal(t, ActionApprove, result.Action)
	assert.Equal(t, 30, result.TimeoutS)
	assert.Equal(t, ActionDeny, result.OnTimeout)
}

func TestService_Respond_UnknownID_ReturnsFalse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	svc := New(vfs, nopKVReader{}, libtracker.NoopTracker{})
	ok := svc.Respond("nonexistent-id", true)
	assert.False(t, ok)
}

func TestService_Evaluate_ResolvesFromKV(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	ctx := context.Background()

	// Write a strict policy (deny-by-default) at a non-default name.
	data := []byte(`{"default_action":"deny","rules":[]}`)
	_, err := vfs.CreateFile(ctx, &vfsservice.File{Name: "hitl-policy-strict.json", Data: data})
	require.NoError(t, err)

	// KV says to use the strict policy.
	svc := New(vfs, fixedKVReader{"hitl-policy-strict.json"}, libtracker.NoopTracker{})
	result, err := svc.Evaluate(ctx, "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, ActionDeny, result.Action, "strict policy (deny-by-default) should deny write_file")
}

func TestService_Evaluate_FallsBackToBuiltinWhenKVEmptyAndFileMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	// No KV entry, no policy file on disk → built-in defaultPolicy() kicks in.
	svc := New(vfs, nopKVReader{}, libtracker.NoopTracker{})
	result, err := svc.Evaluate(context.Background(), "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, ActionApprove, result.Action, "built-in default requires approval for write_file")
}

func TestService_Respond_DuplicateIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vfs := vfsservice.NewLocalFS(dir)
	svc := New(vfs, nopKVReader{}, libtracker.NoopTracker{})
	s := svc.(*service)

	ch := make(chan bool, 1)
	s.mu.Lock()
	s.pending["test-id"] = ch
	s.mu.Unlock()

	ok1 := svc.Respond("test-id", true)
	ok2 := svc.Respond("test-id", false) // duplicate — channel already full
	assert.True(t, ok1)
	assert.False(t, ok2)
	assert.True(t, <-ch)
}
