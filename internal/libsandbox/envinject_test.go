package libsandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Overlay adds new variables and overrides existing ones; the injected values
// always win, and the result is sorted.
func TestUnit_OverlayEnv_AddsAndOverrides(t *testing.T) {
	env := []string{"PATH=/usr/bin", "FOO=old"}
	got := OverlayEnv(env, map[string]string{"FOO": "new", "BAR": "baz"})
	require.Equal(t, []string{"BAR=baz", "FOO=new", "PATH=/usr/bin"}, got)
}

// An empty (or nil) set is a no-op: the environment is returned unchanged, so a
// caller can overlay unconditionally.
func TestUnit_OverlayEnv_EmptySetIsNoop(t *testing.T) {
	env := []string{"PATH=/usr/bin", "FOO=old"}
	require.Equal(t, env, OverlayEnv(env, nil))
	require.Equal(t, env, OverlayEnv(env, map[string]string{}))
}

// A set value of "" exports a defined-but-empty variable; a malformed parent
// entry (no "=") is dropped.
func TestUnit_OverlayEnv_KeepsEmptyValueDropsMalformed(t *testing.T) {
	got := OverlayEnv([]string{"NOEQ", "A=1"}, map[string]string{"EMPTY": ""})
	require.Equal(t, []string{"A=1", "EMPTY="}, got)
}

// Overlay composes with a scrub: strip serve's credentials first, then inject the
// operator's variables on top — the injected value survives even when it shares a
// name the policy would otherwise have dropped.
func TestUnit_OverlayEnv_ComposesAfterScrub(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "AWS_SECRET_ACCESS_KEY=shhh"}
	scrubbed := EnvScrub(ScrubDenySecrets, nil, nil)(parent) // drops the AWS key
	got := OverlayEnv(scrubbed, map[string]string{"AWS_SECRET_ACCESS_KEY": "operator-set", "APP_ENV": "prod"})
	require.Equal(t, []string{"APP_ENV=prod", "AWS_SECRET_ACCESS_KEY=operator-set", "PATH=/usr/bin"}, got)
}
