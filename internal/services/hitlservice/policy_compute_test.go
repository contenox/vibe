package hitlservice_test

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These cover the compute half of the envelope schema (ComputeBounds): a
// well-formed block parses and reads back exactly, an absent one is
// unbounded, and the validate/deny-unknown-fields matrix rejects malformed
// input, falling back to the built-in default policy on Evaluate.

// boundsReader constructs a service over dir and returns it as a
// ComputeBoundsReader (the optional capability a fleet seam type-asserts).
func boundsReader(t *testing.T, dir, fallback string) hitlservice.ComputeBoundsReader {
	t.Helper()
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(dir), testTenant, nopKVReader{}, libtracker.NoopTracker{}, fallback)
	r, ok := svc.(hitlservice.ComputeBoundsReader)
	require.True(t, ok, "the concrete hitlservice must implement ComputeBoundsReader")
	return r
}

func TestUnit_ComputeBounds_ParsesAndReadsBackExactly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicy(t, dir, "envelope.json", []byte(`{
		"default_action":"approve",
		"rules":[{"tools":"local_fs","tool":"write_file","action":"approve"}],
		"compute":{
			"maxTurns":8,
			"maxToolCalls":40,
			"maxTokens":1500000,
			"modelAllowlist":["qwen2.5-coder","llama3.1"],
			"backendAllowlist":["ollama-local"],
			"onExhausted":"finish_stuck"
		}
	}`))

	bounds, err := boundsReader(t, dir, "envelope.json").ComputeBoundsFor(context.Background(), "envelope.json")
	require.NoError(t, err)
	assert.Equal(t, 8, bounds.MaxTurns)
	assert.Equal(t, 40, bounds.MaxToolCalls)
	assert.Equal(t, 1500000, bounds.MaxTokens)
	assert.Equal(t, []string{"qwen2.5-coder", "llama3.1"}, bounds.ModelAllowlist)
	assert.Equal(t, []string{"ollama-local"}, bounds.BackendAllowlist)
	assert.Equal(t, hitlservice.OnExhaustedFinishStuck, bounds.OnExhausted)
}

// TestUnit_ComputeBounds_AbsentBlockIsUnbounded pins that no compute block reads back as the zero (unbounded) bounds.
func TestUnit_ComputeBounds_AbsentBlockIsUnbounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicy(t, dir, "envelope.json", []byte(`{"default_action":"approve","rules":[]}`))

	bounds, err := boundsReader(t, dir, "envelope.json").ComputeBoundsFor(context.Background(), "envelope.json")
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ComputeBounds{}, bounds, "an absent compute block is the zero, unbounded bounds")
	assert.Zero(t, bounds.MaxTurns)
	assert.Zero(t, bounds.MaxToolCalls)
	assert.Zero(t, bounds.MaxTokens)
}

// TestUnit_ComputeBounds_PauseAskParses pins that pause_ask parses, even
// though the enforcement seam still honors it as finish_stuck.
func TestUnit_ComputeBounds_PauseAskParses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicy(t, dir, "envelope.json", []byte(`{"rules":[],"compute":{"maxTurns":3,"onExhausted":"pause_ask"}}`))

	bounds, err := boundsReader(t, dir, "envelope.json").ComputeBoundsFor(context.Background(), "envelope.json")
	require.NoError(t, err)
	assert.Equal(t, hitlservice.OnExhaustedPauseAsk, bounds.OnExhausted)
}

// TestUnit_ComputeBounds_ValidateMatrix_RejectsMalformed pins that every malformed compute block fails the whole policy to load.
func TestUnit_ComputeBounds_ValidateMatrix_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"negative maxTurns":       `{"rules":[],"compute":{"maxTurns":-1}}`,
		"negative maxToolCalls":   `{"rules":[],"compute":{"maxToolCalls":-5}}`,
		"negative maxTokens":      `{"rules":[],"compute":{"maxTokens":-100}}`,
		"maxTurns over cap":       `{"rules":[],"compute":{"maxTurns":1000000}}`,
		"maxToolCalls over cap":   `{"rules":[],"compute":{"maxToolCalls":999999999}}`,
		"unknown onExhausted":     `{"rules":[],"compute":{"onExhausted":"explode"}}`,
		"empty allowlist entry":   `{"rules":[],"compute":{"modelAllowlist":["ok",""]}}`,
		"unknown field (typo)":    `{"rules":[],"compute":{"maxTurn":5}}`,
		"unknown field onExhaust": `{"rules":[],"compute":{"onExhaust":"finish_stuck"}}`,
	}
	for name, body := range cases {
		name, body := name, body
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writePolicy(t, dir, "bad.json", []byte(body))
			_, err := boundsReader(t, dir, "bad.json").ComputeBoundsFor(context.Background(), "bad.json")
			require.Error(t, err, "a malformed compute block must fail the policy to load")
		})
	}
}

func TestUnit_ComputeBounds_ValidateMatrix_AcceptsWellFormed(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"only maxTurns":     `{"rules":[],"compute":{"maxTurns":1}}`,
		"only maxToolCalls": `{"rules":[],"compute":{"maxToolCalls":1}}`,
		"zero fields":       `{"rules":[],"compute":{}}`,
		"finish_stuck":      `{"rules":[],"compute":{"maxTurns":5,"onExhausted":"finish_stuck"}}`,
		"allowlists only":   `{"rules":[],"compute":{"modelAllowlist":["a"],"backendAllowlist":["b"]}}`,
	}
	for name, body := range cases {
		name, body := name, body
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writePolicy(t, dir, "ok.json", []byte(body))
			_, err := boundsReader(t, dir, "ok.json").ComputeBoundsFor(context.Background(), "ok.json")
			require.NoError(t, err, "a well-formed compute block must load")
		})
	}
}

// TestUnit_ComputeBounds_DoesNotAlterActionEvaluation pins that a compute
// block sits alongside the action rules and does not rewrite them.
func TestUnit_ComputeBounds_DoesNotAlterActionEvaluation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicy(t, dir, "envelope.json", []byte(`{
		"default_action":"deny",
		"rules":[{"tools":"webtools","tool":"call","action":"allow"}],
		"compute":{"maxTurns":2}
	}`))
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(dir), testTenant, fixedKVReader{"envelope.json"}, libtracker.NoopTracker{}, "envelope.json")

	allowed, err := svc.Evaluate(context.Background(), "webtools", "call", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, allowed.Action, "the action rule still allows; the compute block is orthogonal")

	denied, err := svc.Evaluate(context.Background(), "local_fs", "write_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionDeny, denied.Action, "default_action still governs an unmatched tool")
}

// TestUnit_ComputeBounds_BadBlockFallsBackToBuiltinOnEvaluate pins that a bad
// compute block fails the policy to load and Evaluate falls back to the
// built-in default.
func TestUnit_ComputeBounds_BadBlockFallsBackToBuiltinOnEvaluate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicy(t, dir, "envelope.json", []byte(`{
		"rules":[{"tools":"local_fs","tool":"write_file","action":"allow"}],
		"compute":{"maxTurns":-1}
	}`))
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(dir), testTenant, fixedKVReader{"envelope.json"}, libtracker.NoopTracker{}, "envelope.json")

	r, err := svc.Evaluate(context.Background(), "local_fs", "write_file", map[string]any{"path": "/tmp/x"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, r.Action,
		"a policy with a malformed compute block must not take effect; the built-in default governs")
}
