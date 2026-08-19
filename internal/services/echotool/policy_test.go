package echotool

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withPolicy attaches a chain's [tools_policies.native-echo] block the way the
// engine does: keyed by the toolset name, as strings.
func withPolicy(args map[string]string) context.Context {
	return taskengine.WithToolsArgs(context.Background(), ToolsProviderName, args)
}

// execCtx runs the tool on a caller-supplied context, which is the only way the
// policy reaches it.
func execCtx(t *testing.T, ctx context.Context, repo taskengine.ToolsRepo, input any) (any, taskengine.DataType) {
	t.Helper()
	out, dt, err := repo.Exec(ctx, time.Now(), input, false,
		&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEcho})
	require.NoError(t, err)
	return out, dt
}

// TestUnit_Policy_EchoCapComesFromTheToolsPolicy pins the args plumbing: the cap
// a call clips to is the chain's, not the compiled default.
func TestUnit_Policy_EchoCapComesFromTheToolsPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		policy  map[string]string
		wantCap int
	}{
		{"no policy keeps the default cap", nil, defaultMaxEchoBytes},
		{"a tighter cap binds", map[string]string{policyMaxEchoBytes: "128"}, 128},
		{"a raised cap admits more", map[string]string{policyMaxEchoBytes: "70000"}, 70000},
		{"past the hard bound clamps to it", map[string]string{policyMaxEchoBytes: "99999999"}, maxMaxEchoBytes},
		{"zero cannot silence the tool", map[string]string{policyMaxEchoBytes: "0"}, minMaxEchoBytes},
		{"negative cannot silence the tool", map[string]string{policyMaxEchoBytes: "-9"}, minMaxEchoBytes},
		{"garbage falls back to the default", map[string]string{policyMaxEchoBytes: "lots"}, defaultMaxEchoBytes},
		{"an empty value falls back to the default", map[string]string{policyMaxEchoBytes: ""}, defaultMaxEchoBytes},
		{"whitespace is tolerated", map[string]string{policyMaxEchoBytes: " 200 "}, 200},
		{"an unrelated key is ignored", map[string]string{"_allowed_dir": "."}, defaultMaxEchoBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Repeat("x", tc.wantCap+500)
			out, _ := execCtx(t, withPolicy(tc.policy), NewTools(), body)
			got := out.(string)
			require.Truef(t, strings.HasPrefix(got, body[:tc.wantCap]), "the echo was cut before the cap")
			assert.Falsef(t, strings.HasPrefix(got, body[:tc.wantCap+1]), "the echo ran past the %d-byte cap", tc.wantCap)
			assert.Contains(t, got, fmt.Sprintf("+%d bytes not echoed", 500))
			// The marker names the key that lifts the cap, not the compiled default.
			assert.Contains(t, got, "tools_policies."+ToolsProviderName+"."+policyMaxEchoBytes)
		})
	}
}

// Under the cap nothing is touched: the clip is a ceiling, not a rewrite.
func TestUnit_Policy_ShortEchoIsVerbatim(t *testing.T) {
	t.Parallel()

	out, dt := execCtx(t, withPolicy(map[string]string{policyMaxEchoBytes: "128"}), NewTools(), "the quick brown fox")
	assert.Equal(t, taskengine.DataTypeString, dt)
	assert.Equal(t, "the quick brown fox", out)
}

// A policy attached to another toolset's key must not reach this one; the args
// context is per-toolset and a leak would let one declaration retune another.
func TestUnit_Policy_IsScopedToThisToolsetsName(t *testing.T) {
	t.Parallel()

	ctx := taskengine.WithToolsArgs(context.Background(), "local_fs", map[string]string{policyMaxEchoBytes: "64"})
	body := strings.Repeat("y", 400)
	out, _ := execCtx(t, ctx, NewTools(), body)
	assert.Equal(t, body, out, "another toolset's policy was applied")
}

// The cut lands on a character boundary, so a clipped echo is still valid UTF-8
// rather than a broken rune the model has to read past.
func TestUnit_Policy_ClipCutsOnACharacterBoundary(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("é", 200) // two bytes per rune
	out, _ := execCtx(t, withPolicy(map[string]string{policyMaxEchoBytes: "101"}), NewTools(), body)
	got := out.(string)
	head, _, ok := strings.Cut(got, "\n… (")
	require.True(t, ok, "the clip marker is missing: %q", got)
	assert.True(t, len(head)%2 == 0, "the cut split a multi-byte character: %d bytes kept", len(head))
	assert.Equal(t, strings.Repeat("é", len(head)/2), head)
}

// The cap also bounds the chat-history form, which is the shape a chain step
// produces — an uncapped echo there would double the conversation it was given.
func TestUnit_Policy_CapAppliesToTheChatHistoryForm(t *testing.T) {
	t.Parallel()

	history := taskengine.ChatHistory{Messages: []taskengine.Message{
		{Role: "user", Content: strings.Repeat("z", 900)},
	}}
	out, dt := execCtx(t, withPolicy(map[string]string{policyMaxEchoBytes: "128"}), NewTools(), history)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)
	got := out.(taskengine.ChatHistory)
	require.Len(t, got.Messages, 2)
	assert.Contains(t, got.Messages[1].Content, "not echoed")
	assert.True(t, strings.HasPrefix(got.Messages[1].Content, "Echo: zzz"))
}

// The descriptor the model reads is rendered from the same limit the call
// enforces, so a policy cannot advertise a cap that is not real.
func TestUnit_Policy_DescriptorAdvertisesTheEffectiveCap(t *testing.T) {
	t.Parallel()

	repo := NewTools()
	ctx := withPolicy(map[string]string{policyMaxEchoBytes: "512"})

	declared, err := repo.GetToolsForToolsByName(ctx, ToolsProviderName)
	require.NoError(t, err)
	props := declared[0].Function.Parameters.(map[string]any)["properties"].(map[string]any)
	desc := props["input"].(map[string]any)["description"].(string)
	assert.Containsf(t, desc, "512 bytes", "the descriptor does not state the active cap:\n%s", desc)

	// The published contract is rendered from the same table.
	docs, err := repo.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	doc, ok := docs[ToolsProviderName]
	require.Truef(t, ok, "no document published under %q, got %v", ToolsProviderName, docs)
	published := doc.Components.Schemas["EchoRequest"].Value.Properties["input"].Value.Description
	assert.Equal(t, desc, published, "published schema and descriptor disagree under policy")
}

// The policy bounds the payload; it is not a second approval gate. A refused
// call is refused by the HITL wrapper above, so nothing here may turn a policy
// value into a denial.
func TestUnit_Policy_NeverRefusesACall(t *testing.T) {
	t.Parallel()

	for _, args := range []map[string]string{
		{policyMaxEchoBytes: "0"},
		{policyMaxEchoBytes: "-1"},
		{policyMaxEchoBytes: "not a number"},
	} {
		out, _, err := NewTools().Exec(withPolicy(args), time.Now(), map[string]any{"input": "still runs"}, false,
			&taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolEcho})
		require.NoErrorf(t, err, "policy %v refused the call", args)
		assert.Equalf(t, "still runs", out, "policy %v suppressed the result", args)
	}
}
