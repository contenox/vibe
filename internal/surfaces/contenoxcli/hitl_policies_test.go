package contenoxcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/hitlservice"
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
