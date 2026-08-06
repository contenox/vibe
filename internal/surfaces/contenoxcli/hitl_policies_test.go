package contenoxcli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/jqtool"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/contenox/contenox/libtracker"
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
		// .git internals are an execution boundary: a planted config key or
		// hook can run as a command. The typed go-git toolset is unaffected.
		"proj/.git/config",
		".git/hooks/pre-commit",
	}
	for _, path := range persistence {
		for _, tool := range []string{"write_file", "sed", "edit_file"} {
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

func TestUnit_SeededBeamPolicy_SecretInvariant(t *testing.T) {
	t.Parallel()
	assertSeededSecretInvariant(t, "hitl-policy-beam.json", hitlPolicyBeam)
}

func TestUnit_SeededDefaultPolicy_SecretInvariant(t *testing.T) {
	t.Parallel()
	assertSeededSecretInvariant(t, "hitl-policy-default.json", hitlPolicyDefault)
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

	// acpx is the untrusted-driver profile: no interactive operator exists to
	// answer "approve", so the policy is pure allow/deny.
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

// TestUnit_SeededPolicies_PassTheEnvelopeVet asserts every shipped preset passes `contenox vet`.
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

// TestUnit_InteractivePolicies_GitToolTiers asserts git reads never ask and git writes always ask.
func TestUnit_InteractivePolicies_GitToolTiers(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
		"hitl-policy-beam.json":    hitlPolicyBeam,
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

// TestUnit_InteractivePolicies_RuleForEveryGitTool asserts every git tool the toolset declares has a rule, so none silently falls through to default_action.
func TestUnit_InteractivePolicies_RuleForEveryGitTool(t *testing.T) {
	t.Parallel()
	declared, err := localtools.NewGitTools(t.TempDir()).GetToolsForToolsByName(context.Background(), localtools.GitToolsName)
	require.NoError(t, err)
	require.NotEmpty(t, declared)

	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
		"hitl-policy-beam.json":    hitlPolicyBeam,
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

// policyRuleDoc is the minimal shape needed to inspect a seeded preset's rules
// for the drift check below.
type policyRuleDoc struct {
	Rules []struct {
		Tools  string `json:"tools"`
		Tool   string `json:"tool"`
		Action string `json:"action"`
		When   []struct {
			Key   string `json:"key"`
			Op    string `json:"op"`
			Value string `json:"value"`
		} `json:"when"`
	} `json:"rules"`
}

// TestUnit_InteractivePolicies_RuleForEveryMutatingLocalFSTool asserts every
// mutating local_fs tool has a deny rule guarding .git/** and hitl-policy*.json
// in the presets that protect those globs, so the next mutating tool added to
// local_fs without matching envelope coverage fails this build instead of
// silently falling through.
func TestUnit_InteractivePolicies_RuleForEveryMutatingLocalFSTool(t *testing.T) {
	t.Parallel()
	mutators := []string{"write_file", "sed", "edit_file"}
	protectedGlobs := []string{"**/hitl-policy*.json", "{**/.git/**,.git/**}"}

	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
		"hitl-policy-strict.json":  hitlPolicyStrict,
		"hitl-policy-beam.json":    hitlPolicyBeam,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var doc policyRuleDoc
			require.NoError(t, json.Unmarshal([]byte(content), &doc))

			for _, glob := range protectedGlobs {
				for _, tool := range mutators {
					found := false
					for _, r := range doc.Rules {
						if r.Tools != "local_fs" || r.Tool != tool || r.Action != "deny" {
							continue
						}
						for _, w := range r.When {
							if w.Key == "path" && w.Op == "glob" && w.Value == glob {
								found = true
							}
						}
					}
					assert.Truef(t, found, "%s has no local_fs.%s deny rule guarding %q", name, tool, glob)
				}
			}
		})
	}
}

// TestUnit_InteractivePolicies_ShellSafeVerbTiers asserts read-only/build verbs run unattended while every other tier keeps asking or denying as before.
func TestUnit_InteractivePolicies_ShellSafeVerbTiers(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
		"hitl-policy-beam.json":    hitlPolicyBeam,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := seededPolicyService(t, name, content)
			ctx := context.Background()

			allow := []map[string]any{
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
				// Every shell git verb asks, read-looking ones included: a git
				// subprocess can execute repo-local config, so no prefix is
				// provably read-only. The no-nag reads are the typed go-git
				// toolset, pinned above.
				{"command": "git", "args": []any{"status"}},
				{"command": "git", "args": []any{"diff", "--stat"}},
				{"command": "git", "args": []any{"log", "--oneline", "-10"}},
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
				{"command": "go", "args": "build; rm -rf /"},
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
		"hitl-policy-beam.json":    hitlPolicyBeam,
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

// TestUnit_PolicyPresetUpgrade asserts untouched presets refresh automatically while hand-edited or unrecorded ones are left alone and reported stale.
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
		// Transition case: predates .preset-state.json but matches this
		// build byte for byte, so it's adopted rather than held back.
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

// TestUnit_InteractivePolicies_JQIsAllowed asserts every declared jq tool is allowed, and that the seeded justification comment still states why.
func TestUnit_InteractivePolicies_JQIsAllowed(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
		"hitl-policy-beam.json":    hitlPolicyBeam,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := seededPolicyService(t, name, content)
			ctx := context.Background()

			// Asserted over the declared tool list, not one name, so a new
			// jq tool is covered automatically.
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

			// Bare boolean, not assert.Contains, so a failure reports the
			// missing phrase instead of dumping the whole preset.
			for _, phrase := range []string{"DEADLINE-BOUNDED", "CANNOT WRITE", "EMPTY object"} {
				assert.Truef(t, strings.Contains(content, phrase),
					"%s: the jq rule must state %q — the allow tier is only defensible while that clause is true", name, phrase)
			}
		})
	}
}

// TestUnit_OraclePolicy_GrantsEveryDeclaredOracleTool asserts the oracle
// envelope allows the tools oracletools actually declares. The preset spells
// them as string literals while the package exports constants, and the
// envelope is default_action:deny — so a rename fails CLOSED (the verdict tool
// denied, the chain never settles, every ask WAITs). Asserted over the
// declared tool list, not one name, so a new oracle tool is covered too.
func TestUnit_OraclePolicy_GrantsEveryDeclaredOracleTool(t *testing.T) {
	t.Parallel()
	svc := seededPolicyService(t, "hitl-policy-oracle.json", hitlPolicyOracle)
	ctx := context.Background()

	// The provider lists its model-facing tools only inside a bound execution.
	bound := oracletools.WithBinding(ctx, oracletools.NewAskBinding("ask-1", `{"askId":"ask-1"}`))
	declared, err := oracletools.New(deniedAnswerer{}).GetToolsForToolsByName(bound, oracletools.ToolsProviderName)
	require.NoError(t, err)
	require.NotEmpty(t, declared)

	names := []string{oracletools.ToolNameVerdictState} // the gate: never advertised, still evaluated
	for _, tool := range declared {
		names = append(names, tool.Function.Name)
	}
	for _, name := range names {
		r, err := svc.Evaluate(ctx, oracletools.ToolsProviderName, name, map[string]any{"verdict": "wait", "askId": "ask-1"})
		require.NoError(t, err)
		assert.Equalf(t, hitlservice.ActionAllow, r.Action,
			"oracle.%s must be allowed — the envelope denies by default, so a missing rule silently WAITs every ask", name)
	}
}

// deniedAnswerer satisfies oracletools.Answerer for listing-only use; New
// panics on a nil answerer and no delivery happens here.
type deniedAnswerer struct{}

func (deniedAnswerer) Answer(context.Context, string, string) error {
	return &oracletools.AnswerRefusedError{Reason: "listing only"}
}

// TestUnit_NoFilePolicyFallback_FailsClosed asserts the no-file fallback has no allow/deny tiers of its own — everything asks, since rules live only in the seeded, readable presets.
func TestUnit_NoFilePolicyFallback_FailsClosed(t *testing.T) {
	t.Parallel()
	// An empty policy dir forces the built-in fallback.
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()),
		testTenant, nopKV{}, libtracker.NoopTracker{}, "hitl-policy-default.json")
	ctx := context.Background()
	for _, call := range []struct {
		tools, tool string
		args        map[string]any
	}{
		{jqtool.ToolsProviderName, jqtool.ToolQuery, map[string]any{"path": "chain.json", "filter": "."}},
		{"local_fs", "read_file", map[string]any{"path": "src/main.go"}},
		{"local_shell", "local_shell", map[string]any{"command": "ls"}},
		{"gointel", "go_symbols", map[string]any{"path": "."}},
	} {
		r, err := svc.Evaluate(ctx, call.tools, call.tool, call.args)
		require.NoError(t, err)
		assert.Equalf(t, hitlservice.ActionApprove, r.Action,
			"%s.%s: with no loadable policy file, everything asks — allow tiers exist only in seeded, readable envelopes", call.tools, call.tool)
	}
}

// TestUnit_InteractivePolicies_RuleForEveryReadOnlyLocalFSTool asserts every
// read-only local_fs tool (the toolset's declared Supports() list minus the
// three mutators) evaluates to allow on every interactive preset, plus
// strict.json's read tier. find_files shipped without a rule beside
// list_dir's, so a plain read triggered an approval card in production; this
// pins the whole read-only set so the next read-only tool added without a
// matching rule fails the build instead of surprising a human with a card.
func TestUnit_InteractivePolicies_RuleForEveryReadOnlyLocalFSTool(t *testing.T) {
	t.Parallel()
	declared, err := localtools.NewLocalFSTools(t.TempDir(), nil).Supports(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, declared)

	mutators := map[string]bool{"write_file": true, "sed": true, "edit_file": true}
	var readOnly []string
	for _, name := range declared {
		if name == localtools.LocalFSToolsName || mutators[name] {
			continue
		}
		readOnly = append(readOnly, name)
	}
	require.NotEmpty(t, readOnly)

	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
		"hitl-policy-beam.json":    hitlPolicyBeam,
		"hitl-policy-strict.json":  hitlPolicyStrict,
		"hitl-policy-acpx.json":    hitlPolicyACPX,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := seededPolicyService(t, name, content)
			ctx := context.Background()
			for _, tool := range readOnly {
				r, err := svc.Evaluate(ctx, "local_fs", tool, nil)
				require.NoError(t, err)
				assert.Equalf(t, hitlservice.ActionAllow, r.Action,
					"%s: local_fs.%s is a read-only tool and must be in the allow tier beside list_dir's", name, tool)
			}
		})
	}
}

// TestUnit_InteractivePolicies_JSAndPythonShellTiers asserts JS/Python build-and-test verbs run unasked while their mutating, network, or arbitrary-execution siblings still ask.
func TestUnit_InteractivePolicies_JSAndPythonShellTiers(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"hitl-policy-default.json": hitlPolicyDefault,
		"hitl-policy-acp.json":     hitlPolicyACP,
		"hitl-policy-beam.json":    hitlPolicyBeam,
	} {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := seededPolicyService(t, name, content)
			ctx := context.Background()

			allowed := []map[string]any{
				{"command": "pytest", "args": "-q tests/"},
				{"command": "python", "args": []any{"-m", "pytest", "tests/"}},
				{"command": "python3", "args": []any{"-m", "pytest"}},
				{"command": "npm", "args": "test"},
				{"command": "npm", "args": []any{"ls"}},
				{"command": "vitest", "args": []any{"run"}},
				{"command": "jest", "args": "--ci"},
				{"command": "mypy", "args": "--strict app/"},
				{"command": "ruff", "args": []any{"check", "."}},
				{"command": "tsc", "args": "--noEmit"},
				{"command": "eslint", "args": []any{"src/"}},
				{"command": "pip", "args": []any{"list"}},
				{"command": "pip", "args": "show requests"},
			}
			for _, args := range allowed {
				r, err := svc.Evaluate(ctx, "local_shell", "local_shell", args)
				require.NoError(t, err)
				assert.Equal(t, hitlservice.ActionAllow, r.Action, "%s: safe verb must not ask: %v", name, args)
			}

			// The documented DELIBERATELY ABSENT set: each must keep asking.
			stillAsks := []map[string]any{
				{"command": "pip", "args": "install requests"},       // network + env mutation
				{"command": "npm", "args": "install eslint"},         // network + env mutation
				{"command": "npx", "args": "eslint ."},               // install-on-missing
				{"command": "npx", "args": []any{"tsc", "--noEmit"}}, // install-on-missing
				{"command": "uv", "args": "run pytest"},              // install-on-missing
				{"command": "python", "args": "-c print(1)"},         // arbitrary execution
				{"command": "node", "args": "evil.js"},               // arbitrary execution
				{"command": "vitest"},                                // watch mode never exits
				{"command": "ruff", "args": "format ."},              // rewrites sources
				{"command": "tsc"},                                   // bare tsc emits files
				{"command": "pytest2"},                               // token-wise, not substring
			}
			for _, args := range stillAsks {
				r, err := svc.Evaluate(ctx, "local_shell", "local_shell", args)
				require.NoError(t, err)
				assert.NotEqual(t, hitlservice.ActionAllow, r.Action, "%s: must not be auto-allowed: %v", name, args)
			}

			// The ask-always tier is untouched by the wider allow tier beside
			// it, including through a compound line that STARTS allowlisted.
			for _, args := range []map[string]any{
				{"command": "rm", "args": "-rf /tmp/x"},
				{"command": "sudo", "args": "ls"},
				{"command": "pytest && rm -rf /tmp/x"},
			} {
				r, err := svc.Evaluate(ctx, "local_shell", "local_shell", args)
				require.NoError(t, err)
				assert.NotEqual(t, hitlservice.ActionAllow, r.Action, "%s: destructive path must never be auto-allowed: %v", name, args)
			}
		})
	}
}
