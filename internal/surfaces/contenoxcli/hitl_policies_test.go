package contenoxcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/jqtool"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTenant = "00000000-0000-0000-0000-000000000001"

type nopKV struct{}

func (nopKV) GetKV(_ context.Context, _ string, _ any) error { return errors.New("not found") }

func seededPolicyService(t *testing.T, name, content string) hitlservice.Service {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	return hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(dir), testTenant, nopKV{}, libtracker.NoopTracker{}, name)
}

func assertSeededSecretInvariant(t *testing.T, name, content string) {
	t.Helper()
	svc := seededPolicyService(t, name, content)
	ctx := context.Background()

	quarantined := []string{
		"/home/u/.ssh/id_rsa",
		"/home/u/.gnupg/secring.gpg",
		"/home/u/.aws/credentials",
		"/home/u/.config/gcloud/access_tokens.db",
		"/home/u/.password-store/work.gpg",
		"/home/u/Library/Keychains/login.keychain-db",
		"/home/u/.mozilla/firefox/p/cookies.sqlite",
		"/home/u/.git-credentials",
		"/home/u/keys/id_ed25519",
		"/home/u/funds.kdbx",
	}
	for _, path := range quarantined {
		for _, tool := range []string{"read_file", "read_file_range", "grep", "stat_file", "count_stats"} {
			r, err := svc.Evaluate(ctx, "local_fs", tool, map[string]any{"path": path})
			require.NoError(t, err)
			assert.Equal(t, hitlservice.ActionDeny, r.Action, "%s: local_fs.%s(%q) must be denied", name, tool, path)
		}
	}

	persistence := []string{
		"/home/u/.ssh/authorized_keys",
		"/home/u/.config/autostart/x.desktop",
		"/home/u/Library/LaunchAgents/com.x.plist",
		"/home/u/.bashrc",
		"/etc/cron.d/x",
		"/usr/bin/x",
		"/home/u/.contenox/hitl-policy-acp.json",
		"proj/hitl-policy-strict.json",
	}
	for _, path := range persistence {
		for _, tool := range []string{"write_file", "sed"} {
			r, err := svc.Evaluate(ctx, "local_fs", tool, map[string]any{"path": path})
			require.NoError(t, err)
			assert.Equal(t, hitlservice.ActionDeny, r.Action, "%s: local_fs.%s(%q) must be denied", name, tool, path)
		}
	}

	for _, path := range []string{"src/main.go", "/home/u/proj/README.md", "internal/foo_test.go"} {
		r, err := svc.Evaluate(ctx, "local_fs", "read_file", map[string]any{"path": path})
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionAllow, r.Action, "%s: ordinary read %q must stay allowed", name, path)
	}

	for _, path := range []string{"deploy/server.pem", "config/tls.key", "proj/.env", "proj/.env.production"} {
		r, err := svc.Evaluate(ctx, "local_fs", "read_file", map[string]any{"path": path})
		require.NoError(t, err)
		assert.NotEqual(t, hitlservice.ActionAllow, r.Action, "%s: sensitive project file %q must never be auto-allowed (approve on interactive policies, deny on untrusted-driver acpx)", name, path)
	}
}

func TestUnit_SeededACPPolicy_SecretInvariant(t *testing.T) {
	t.Parallel()
	assertSeededSecretInvariant(t, "hitl-policy-acp.json", hitlPolicyACP)
}

func TestUnit_SeededStrictPolicy_SecretInvariant(t *testing.T) {
	t.Parallel()
	assertSeededSecretInvariant(t, "hitl-policy-strict.json", hitlPolicyStrict)
}

func TestUnit_SeededACPXPolicy_SecretInvariantAndHeavyDeltas(t *testing.T) {
	t.Parallel()
	assertSeededSecretInvariant(t, "hitl-policy-acpx.json", hitlPolicyACPX)

	svc := seededPolicyService(t, "hitl-policy-acpx.json", hitlPolicyACPX)
	ctx := context.Background()

	// acpx is the untrusted-driver profile. There is no interactive operator on
	// a non-interactive channel (e.g. OpenClaw's Telegram bridge), so "approve"
	// is not a valid action class here: it would only ever degrade to deny or
	// allow at the host. The policy is therefore pure allow/deny — every
	// mutating capability is an explicit deny, not an unhonorable "approve".
	deny := map[string][2]string{
		"shell":      {"local_shell", "local_shell"},
		"web_post":   {"webtools", "web_post"},
		"web_put":    {"webtools", "web_put"},
		"web_patch":  {"webtools", "web_patch"},
		"web_delete": {"webtools", "web_delete"},
		"web_get":    {"webtools", "web_get"},
		"web_head":   {"webtools", "web_head"},
		"write_file": {"local_fs", "write_file"},
		"sed":        {"local_fs", "sed"},
	}
	for label, tt := range deny {
		r, err := svc.Evaluate(ctx, tt[0], tt[1], nil)
		require.NoError(t, err)
		assert.Equal(t, hitlservice.ActionDeny, r.Action, "acpx must deny %s (no approve tier on an untrusted driver)", label)
	}

	// Reads still pass (containment, not lockout).
	r, err := svc.Evaluate(ctx, "local_fs", "read_file", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action, "acpx must allow plain reads")

	// Floor is deny: an untrusted driver gets least privilege. An unaccounted
	// tool is denied, never silently approved.
	r, err = svc.Evaluate(ctx, "some_unaccounted_mcp", "arbitrary_tool", nil)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionDeny, r.Action, "acpx default_action must be deny (untrusted driver, least privilege)")
}

// TestUnit_SeededPolicies_PassTheEnvelopeVet runs the authoring-time validator
// `contenox vet` runs over every shipped preset. It is stricter than the load
// path (unknown fields, dead timeouts, patterns that can never match), so a
// preset edited by hand fails here rather than silently disarming a rule.
func TestUnit_SeededPolicies_PassTheEnvelopeVet(t *testing.T) {
	t.Parallel()
	for _, p := range HITLPolicyPresets {
		p := p
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, hitlservice.VetPolicy([]byte(p.Content)), "%s must pass the envelope vet", p.Name)
		})
	}
}

// TestUnit_InteractivePolicies_GitToolTiers pins the git envelope on the two
// presets a person is sitting in front of: looking at the repository never
// interrupts, changing it always asks.
func TestUnit_InteractivePolicies_GitToolTiers(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := seededPolicyService(t, name, content)
			ctx := context.Background()

			for _, tool := range []string{"git_status", "git_diff", "git_log", "git_show", "git_branch_list", "git_blame"} {
				r, err := svc.Evaluate(ctx, "git", tool, map[string]any{})
				require.NoError(t, err)
				assert.Equal(t, hitlservice.ActionAllow, r.Action, "%s: git.%s must never nag", name, tool)
			}
			for _, tool := range []string{"git_add", "git_commit", "git_checkout_branch", "git_restore"} {
				r, err := svc.Evaluate(ctx, "git", tool, map[string]any{})
				require.NoError(t, err)
				assert.Equal(t, hitlservice.ActionApprove, r.Action, "%s: git.%s changes the repository and must ask", name, tool)
			}
		})
	}
}

// TestUnit_InteractivePolicies_RuleForEveryGitTool closes the drift seam between
// the toolset and the envelopes that gate it: a git tool added or renamed in
// localtools without a rule here would fall through to default_action, which on
// these presets means it silently starts asking (or, on a permissive one,
// silently stops). The toolset is the source of truth; the presets must name
// every operation it declares.
func TestUnit_InteractivePolicies_RuleForEveryGitTool(t *testing.T) {
	t.Parallel()
	declared, err := localtools.NewGitTools(t.TempDir()).GetToolsForToolsByName(context.Background(), localtools.GitToolsName)
	require.NoError(t, err)
	require.NotEmpty(t, declared)

	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, tool := range declared {
				assert.Containsf(t, content, `"tool": "`+tool.Function.Name+`"`,
					"%s has no rule for git.%s — it would fall through to default_action", name, tool.Function.Name)
			}
		})
	}
}

// TestUnit_InteractivePolicies_ShellSafeVerbTiers pins the shell tiers: the
// read-only and build verbs run unattended, and every tier above and below them
// keeps its old verdict.
func TestUnit_InteractivePolicies_ShellSafeVerbTiers(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := seededPolicyService(t, name, content)
			ctx := context.Background()

			allow := []map[string]any{
				{"command": "git", "args": []any{"status"}},
				{"command": "git", "args": []any{"diff", "--stat"}},
				{"command": "git", "args": []any{"log", "--oneline", "-10"}},
				{"command": "go", "args": []any{"build", "./..."}},
				{"command": "go", "args": []any{"test", "-short", "./internal/..."}},
				{"command": "ls", "args": []any{"-la"}},
				{"command": "cat", "args": []any{"go.mod"}},
				{"command": "rg", "args": []any{"TODO", "internal"}},
			}
			for _, args := range allow {
				r, err := svc.Evaluate(ctx, "local_shell", "local_shell", args)
				require.NoError(t, err)
				assert.Equal(t, hitlservice.ActionAllow, r.Action, "%s: %v is a safe verb and must not nag", name, args)
			}

			ask := []map[string]any{
				{"command": "git", "args": []any{"clean", "-fd"}},
				{"command": "git", "args": []any{"reset", "--hard"}},
				{"command": "git", "args": []any{"push"}},
				{"command": "go", "args": []any{"run", "./cmd/x"}},
				{"command": "gofmt", "args": []any{"-w", "."}},
				{"command": "find", "args": []any{".", "-delete"}},
				{"command": "rm", "args": []any{"-rf", "build"}},
				{"command": "sudo", "args": []any{"ls"}},
				{"command": "mv", "args": []any{"a", "b"}},
				{"command": "python3"},
				{"command": "curl", "args": []any{"https://example.com"}},
				// An allowlisted verb carrying the rest of a shell line with it.
				{"command": "git", "args": "status; rm -rf /"},
				{"command": "ls", "args": []any{"-la"}, "shell": true},
				{"command": "echo", "args": "$(id)"},
			}
			for _, args := range ask {
				r, err := svc.Evaluate(ctx, "local_shell", "local_shell", args)
				require.NoError(t, err)
				assert.Equal(t, hitlservice.ActionApprove, r.Action, "%s: %v must still ask", name, args)
			}

			for _, cmd := range []string{"mkfs", "shred", "wipefs"} {
				r, err := svc.Evaluate(ctx, "local_shell", "local_shell", map[string]any{"command": cmd})
				require.NoError(t, err)
				assert.Equal(t, hitlservice.ActionDeny, r.Action, "%s: %s stays denied", name, cmd)
			}
		})
	}
}

func TestUnit_InteractivePoliciesRequireApprovalForPlainShellFallback(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-dev.json":     hitlPolicyDev,
		"hitl-policy-acp.json":     hitlPolicyACP,
		"hitl-policy-strict.json":  hitlPolicyStrict,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := seededPolicyService(t, name, content)
			r, err := svc.Evaluate(context.Background(), "local_shell", "local_shell", map[string]any{"command": "python3"})
			require.NoError(t, err)
			assert.Equal(t, hitlservice.ActionApprove, r.Action, "%s must not auto-allow plain shell fallback", name)
		})
	}
}

// TestUnit_PolicyPresetUpgrade covers the deployment gap that made a shipped
// read tool ask for approval on an existing install: presets were written
// once and never refreshed, so an envelope predating a toolset had no rule
// for it and every call fell through to default_action.
func TestUnit_PolicyPresetUpgrade(t *testing.T) {
	name := HITLPolicyPresets[0].Name

	t.Run("untouched preset is upgraded", func(t *testing.T) {
		dir := t.TempDir()
		// A previous build's file, recorded as ours.
		old := `{"default_action":"approve","rules":[]}`
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(old), 0644))
		writePresetState(dir, map[string]string{name: presetSHA(old)})

		stale, err := upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.Empty(t, stale)

		got, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, HITLPolicyPresets[0].Content, string(got), "an unedited preset must be refreshed")
	})

	t.Run("hand-edited preset is kept and reported", func(t *testing.T) {
		dir := t.TempDir()
		edited := `{"default_action":"deny","rules":[]}`
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(edited), 0644))
		writePresetState(dir, map[string]string{name: presetSHA("something else entirely")})

		stale, err := upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.Contains(t, stale, name)

		got, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, edited, string(got), "the operator's edit must survive")
	})

	t.Run("unrecorded preset is kept and reported", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(`{"default_action":"approve"}`), 0644))

		stale, err := upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.Contains(t, stale, name, "a preset with no provenance record is treated as the operator's")
	})

	t.Run("a byte-identical preset with no record is adopted", func(t *testing.T) {
		// The transition case: an install that predates .preset-state.json but
		// whose preset happens to match this build. Nothing is written, nothing
		// is reported — the hash is simply recorded, so the NEXT build upgrades
		// this install automatically instead of holding it back forever.
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(HITLPolicyPresets[0].Content), 0644))
		require.NoFileExists(t, filepath.Join(dir, presetStateFile))

		stale, err := upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.NotContains(t, stale, name, "a preset identical to this build's is provably ours")
		require.Equal(t, presetSHA(HITLPolicyPresets[0].Content), readPresetState(dir)[name],
			"the adoption must be recorded, or the next upgrade repeats this stand-off")
	})

	t.Run("fresh install writes and records every preset", func(t *testing.T) {
		dir := t.TempDir()
		stale, err := upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.Empty(t, stale)

		state := readPresetState(dir)
		for _, p := range HITLPolicyPresets {
			require.Equal(t, presetSHA(p.Content), state[p.Name], "every written preset is recorded")
		}
		// A second run is a no-op that still reports nothing stale.
		stale, err = upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.Empty(t, stale)
	})
}

// TestUnit_InteractivePolicies_JQIsAllowed pins the seeded jq envelope on a
// FRESH policy directory — the seeded file is written to a temp dir and loaded
// through the real service, so this asserts what an operator actually gets, not
// what the JSON literal says.
//
// The tier is allow BY CONSTRUCTION and the reasons are structural (jq_query
// reads a file read_file already reaches, writes nothing, reaches no network,
// and is deadline-bounded including recursion). If any of those stops being
// true, this test is the tripwire: it fails loudly rather than letting a tool
// that gained a capability keep the tier it earned without one.
func TestUnit_InteractivePolicies_JQIsAllowed(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := seededPolicyService(t, name, content)
			ctx := context.Background()

			// Every tool the toolset DECLARES must be allowed — the same
			// drift guard the git test applies, so a second jq tool added
			// later cannot inherit this rule silently... it would, since the
			// rule is toolset-wide, which is exactly why the assertion is
			// written over the declared list rather than over one name.
			declared, err := jqtool.NewTools(t.TempDir()).GetToolsForToolsByName(ctx, jqtool.ToolsProviderName)
			require.NoError(t, err)
			require.NotEmpty(t, declared)
			for _, tool := range declared {
				r, err := svc.Evaluate(ctx, jqtool.ToolsProviderName, tool.Function.Name,
					map[string]any{"path": "chain.json", "filter": "."})
				require.NoError(t, err)
				assert.Equalf(t, hitlservice.ActionAllow, r.Action,
					"%s: jq.%s must never nag — it reads a file read_file already reaches and writes nothing",
					name, tool.Function.Name)
			}

			// The justification comment is load-bearing documentation, not
			// decoration: it is what the next reader weighs the tier against.
			// Asserted with a bare boolean rather than assert.Contains so a
			// failure reports the missing phrase instead of dumping the whole
			// 40 KB preset into the test log.
			for _, phrase := range []string{"DEADLINE-BOUNDED", "CANNOT WRITE", "EMPTY object"} {
				assert.Truef(t, strings.Contains(content, phrase),
					"%s: the jq rule must state %q — the allow tier is only defensible while that clause is true", name, phrase)
			}
		})
	}
}

// TestUnit_DefaultPolicy_JQMatchesTheSeededPresets closes the drift seam between
// the built-in fallback policy (hitlservice.defaultPolicy, used when no preset
// file is present) and the shipped JSON. A tool allowed by the file and unruled
// in the fallback would nag on exactly the installs that have no policy dir yet.
func TestUnit_DefaultPolicy_JQMatchesTheSeededPresets(t *testing.T) {
	t.Parallel()
	// An empty policy dir forces the built-in fallback.
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()),
		testTenant, nopKV{}, libtracker.NoopTracker{}, "hitl-policy-default.json")
	r, err := svc.Evaluate(context.Background(), jqtool.ToolsProviderName, jqtool.ToolQuery,
		map[string]any{"path": "chain.json", "filter": "."})
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionAllow, r.Action,
		"hitlservice.defaultPolicy() must allow jq exactly as the seeded presets do")
}
