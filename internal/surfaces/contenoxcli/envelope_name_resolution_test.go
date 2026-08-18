package contenoxcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderShippedEnvelopesForTest writes every declared envelope into
// contenoxDir/.generated exactly as syncEnvelopePolicies does at startup, so the
// resolution below reads what a real run would.
func renderShippedEnvelopesForTest(t *testing.T, contenoxDir string) {
	t.Helper()
	cfg, err := agentdecl.Shipped()
	require.NoError(t, err)
	generated := filepath.Join(contenoxDir, agentdecl.GeneratedDirName)
	for _, name := range cfg.EnvelopeNames() {
		_, _, err := agentdecl.SyncEnvelopePolicy(cfg, name, generated, agentdecl.ConfigFilename)
		require.NoErrorf(t, err, "sync envelope %q", name)
	}
}

// nameResolvedService is the HITL service the runtime wires: an FS source over
// policyDirs, defaulting to the default-envelope file. HOME is expected to be
// pinned to contenoxDir so policyDirs is hermetic.
func nameResolvedService(t *testing.T, contenoxDir string) hitlservice.Service {
	t.Helper()
	src := hitlservice.NewFSPolicySource(policyDirs(contenoxDir)...)
	return hitlservice.NewWithDefaultPolicy(src, testTenant, nopKV{}, libtracker.NoopTracker{},
		agentdecl.EnvelopePolicyFile(chatProfileEnvelope))
}

// TestUnit_ShippedEnvelopes_ResolvedByBareName_GateCredentials closes the
// ratchet's blind spot: the ratchet compares two documents through
// RenderEnvelopePolicy and never resolves a name. This drives a BARE policy name
// — the form /policy and the config KV store — through the runtime's real
// resolver (WithPolicyName -> Evaluate -> FS source over policyDirs) and asserts
// the credential quarantine, write wall and shell blacklist still gate.
func TestUnit_ShippedEnvelopes_ResolvedByBareName_GateCredentials(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	renderShippedEnvelopesForTest(t, tmp)
	svc := nameResolvedService(t, tmp)
	ctx := context.Background()

	deny := func(policyName, tools, tool string, args map[string]any) {
		t.Helper()
		r, err := svc.Evaluate(hitlservice.WithPolicyName(ctx, policyName), tools, tool, args)
		require.NoError(t, err)
		// The bare name must resolve to its own envelope, not be rescued by the
		// default fallback (which also quarantines credentials and would mask a
		// resolution regression). Evaluate echoes the gating policy name, so a
		// resolved run reports the bare name back; a fallback reports the default file.
		assert.Equalf(t, policyName, r.PolicyName,
			"bare policy %q must resolve to its own envelope, gated on %q instead (fallback masking?)", policyName, r.PolicyName)
		assert.Equalf(t, hitlservice.ActionDeny, r.Action,
			"policy %q: %s.%s(%v) must be DENY, got %s (policy resolved to %q)",
			policyName, tools, tool, args, r.Action, r.PolicyName)
	}

	const credential = "/home/u/.ssh/id_rsa" // matches the read_only quarantine.
	const etcPasswd = "/etc/passwd"
	// Bare names, exactly as `/policy strict` and `config set hitl-policy-name strict` store them.
	for _, name := range []string{"strict", "acpx"} {
		for _, tool := range []string{"read_file", "write_file", "sed"} {
			deny(name, "local_fs", tool, map[string]any{"path": credential})
		}
		deny(name, "local_fs", "write_file", map[string]any{"path": etcPasswd})
		deny(name, "local_fs", "sed", map[string]any{"path": etcPasswd})
		deny(name, "local_shell", "local_shell", map[string]any{"command": "mkfs /dev/sda"})
	}
}

// TestUnit_HITL_UnresolvableName_FallsToDefaultNotApproveAll pins the
// load-failure fallback the finding named as untested: an empty or unresolvable
// policy name gates on the default file — which itself quarantines credentials —
// never on the approve-everything built-in.
func TestUnit_HITL_UnresolvableName_FallsToDefaultNotApproveAll(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	renderShippedEnvelopesForTest(t, tmp)
	svc := nameResolvedService(t, tmp)
	ctx := context.Background()
	credential := map[string]any{"path": "/home/u/.ssh/id_rsa"}
	defaultFile := agentdecl.EnvelopePolicyFile(chatProfileEnvelope)

	// No override, empty KV: the default file gates, and it denies the key.
	empty, err := svc.Evaluate(ctx, "local_fs", "read_file", credential)
	require.NoError(t, err)
	assert.Equal(t, hitlservice.ActionDeny, empty.Action)
	assert.Equal(t, defaultFile, empty.PolicyName)

	// A name resolving to no file must gate on the default, not collapse to approve-all.
	typo, err := svc.Evaluate(hitlservice.WithPolicyName(ctx, "strikt"), "local_fs", "read_file", credential)
	require.NoError(t, err)
	assert.Equalf(t, hitlservice.ActionDeny, typo.Action,
		"an unresolvable policy name must not downgrade a credential read to approve; got %s", typo.Action)
	assert.Equalf(t, defaultFile, typo.PolicyName,
		"an unresolvable name must fall to the default file, never to the approve-all built-in")
}

// TestUnit_NameResolution_NegativeControl_CredentialQuarantineIsLoadBearing
// proves the DENY the resolution test asserts comes from the credential
// always_deny block: strip it, resolve the same bare "strict", and reading a
// private key is no longer denied.
func TestUnit_NameResolution_NegativeControl_CredentialQuarantineIsLoadBearing(t *testing.T) {
	var strict hitlservice.Policy
	require.NoError(t, json.Unmarshal([]byte(renderedEnvelopePolicy(t, "strict")), &strict))

	kept := make([]hitlservice.Rule, 0, len(strict.Rules))
	dropped := 0
	for _, r := range strict.Rules {
		// The credential quarantine is the only local_fs/* deny in strict.
		if r.Tools == "local_fs" && r.Tool == "*" && r.Action == hitlservice.ActionDeny {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	require.Positivef(t, dropped, "strict must carry local_fs/* credential deny rules for the control to remove")
	strict.Rules = kept
	weakened, err := json.Marshal(&strict)
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, agentdecl.EnvelopePolicyFile("strict")), weakened, 0o644))
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(dir), testTenant, nopKV{},
		libtracker.NoopTracker{}, agentdecl.EnvelopePolicyFile("strict"))

	r, err := svc.Evaluate(hitlservice.WithPolicyName(context.Background(), "strict"),
		"local_fs", "read_file", map[string]any{"path": "/home/u/.ssh/id_rsa"})
	require.NoError(t, err)
	assert.NotEqualf(t, hitlservice.ActionDeny, r.Action,
		"with the credential quarantine removed, reading a private key must no longer be DENY (got %s); the resolution test's DENY is therefore load-bearing", r.Action)
}
