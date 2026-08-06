package hitlservice_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover identity (H2) and integrity (H3): a command_prefix_allowlist
// pins a NAME, and PATH decides what that name is. Every case is a WITHDRAWN
// allow — the gate can only refuse more, never allow more.

// fakeTool writes an executable named `name` into dir and returns its path.
// On Windows a bare name only resolves through PATHEXT, so the fixture is a
// .cmd there — verified against where.exe, which lists foo.CMD before foo.JS
// for PATHEXT ".COM;.EXE;.BAT;.CMD;...".
func fakeTool(t *testing.T, dir, name, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	file := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		file += ".cmd"
		body = "@echo off\r\necho " + body + "\r\n"
	} else {
		body = "#!/bin/sh\necho " + body + "\n"
	}
	require.NoError(t, os.WriteFile(file, []byte(body), 0o755))
	return file
}

// realPathOf is what the evaluator will compare against: the symlink-resolved
// absolute path. Declaring anything else is the operator error the refusal
// messages teach.
func realPathOf(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return filepath.Clean(resolved)
}

func sha256Of(t *testing.T, path string) string {
	t.Helper()
	_, sum, err := hitlservice.ResolveTrustedBinary(path)
	require.NoError(t, err)
	return sum
}

// trustPolicy writes a one-rule allow envelope gated by a trusted_binaries
// block, mirroring the shape an operator authors.
func trustPolicy(t *testing.T, prefixes string, tb map[string]any) hitlservice.PolicyEvaluator {
	t.Helper()
	dir := t.TempDir()
	doc := map[string]any{
		"default_action": "approve",
		"rules": []any{map[string]any{
			"tools": "local_shell", "tool": "local_shell", "action": "allow",
			"when": []any{map[string]any{"key": "command", "op": "command_prefix_allowlist", "value": prefixes}},
		}},
	}
	if tb != nil {
		doc["trusted_binaries"] = tb
	}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	writePolicy(t, dir, "hitl-policy.json", raw)
	return hitlservice.New(hitlservice.NewFSPolicySource(dir), testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})
}

func evalTrust(t *testing.T, svc hitlservice.PolicyEvaluator, args map[string]any) hitlservice.EvaluationResult {
	t.Helper()
	r, err := svc.Evaluate(hitlservice.WithShellKind(context.Background(), "sh"), "local_shell", "local_shell", args)
	require.NoError(t, err)
	return r
}

// TestUnit_TrustedBinaries_PathSubstitutionIsRefused is the headline case: a
// writable directory earlier in PATH aliasing an allowlisted name must not
// convert the allow rule into blessed arbitrary execution.
func TestUnit_TrustedBinaries_PathSubstitutionIsRefused(t *testing.T) {
	root := t.TempDir()
	trustedDir := filepath.Join(root, "trusted")
	evilDir := filepath.Join(root, "evil")
	good := fakeTool(t, trustedDir, "mytool", "the real one")
	fakeTool(t, evilDir, "mytool", "PWNED")

	svc := trustPolicy(t, "mytool", map[string]any{
		"dirs":   []string{realPathOf(t, trustedDir)},
		"hashes": map[string]string{realPathOf(t, good): sha256Of(t, good)},
	})

	// Control: with only the trusted dir on PATH, the allow stands.
	t.Setenv("PATH", trustedDir)
	assert.Equal(t, hitlservice.ActionAllow, evalTrust(t, svc, map[string]any{"command": "mytool"}).Action,
		"the declared binary on a clean PATH must still be allowed")

	// The attack: the same name, resolved out of a writable directory placed
	// earlier in PATH.
	t.Setenv("PATH", evilDir+string(os.PathListSeparator)+trustedDir)
	r := evalTrust(t, svc, map[string]any{"command": "mytool"})
	assert.Equal(t, hitlservice.ActionApprove, r.Action,
		"a PATH-prepended alias of an allowlisted name must not inherit its allow")
	assert.Contains(t, r.Detail, "trusted_binaries.dirs",
		"the refusal must say the binary came from somewhere undeclared")
	assert.Contains(t, r.Detail, "evil", "the refusal must name the binary it refused")

	// The compound-line (structural) path is gated identically.
	rc := evalTrust(t, svc, map[string]any{"command": "mytool && mytool"})
	assert.Equal(t, hitlservice.ActionApprove, rc.Action,
		"the structural upgrade path must be gated by the same declarations")

	// Hashes ALONE stop the substitution too — the planted binary is at a
	// different path, so it has no declared hash. Pinned because the docs say so.
	hashesOnly := trustPolicy(t, "mytool", map[string]any{
		"hashes": map[string]string{realPathOf(t, good): sha256Of(t, good)},
	})
	rh := evalTrust(t, hashesOnly, map[string]any{"command": "mytool"})
	assert.Equal(t, hitlservice.ActionApprove, rh.Action,
		"a declared-hashes block with no dirs must still refuse a substituted binary")
	assert.Contains(t, rh.Detail, "has no declared hash")

	t.Setenv("PATH", trustedDir)
	assert.Equal(t, hitlservice.ActionAllow, evalTrust(t, hashesOnly, map[string]any{"command": "mytool"}).Action,
		"and must still allow the declared one")
}

// TestUnit_TrustedBinaries_HashMismatchIsRefused pins the documented failure
// text for a swapped or tampered binary at a declared path.
func TestUnit_TrustedBinaries_HashMismatchIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	tool := fakeTool(t, dir, "mytool", "the real one")
	real := realPathOf(t, tool)
	declared := sha256Of(t, tool)

	svc := trustPolicy(t, "mytool", map[string]any{
		"dirs":   []string{realPathOf(t, dir)},
		"hashes": map[string]string{real: declared},
	})
	t.Setenv("PATH", dir)
	require.Equal(t, hitlservice.ActionAllow, evalTrust(t, svc, map[string]any{"command": "mytool"}).Action)

	// Same path, same directory, different bytes: the swap H3 exists for.
	fakeTool(t, dir, "mytool", "PWNED")
	r := evalTrust(t, svc, map[string]any{"command": "mytool"})
	assert.Equal(t, hitlservice.ActionApprove, r.Action, "a tampered binary must be refused, never warned about")
	assert.Contains(t, r.Detail, "does not match the declared hash — re-declare after verifying the upgrade, or investigate",
		"the refusal must be the documented one, verbatim")
	assert.Contains(t, r.Detail, real, "the refusal must name the path")
}

// TestUnit_TrustedBinaries_RefreshFixesAMismatch pins the upgrade workflow:
// re-reading the binary and rewriting the declaration restores the allow.
func TestUnit_TrustedBinaries_RefreshFixesAMismatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	tool := fakeTool(t, dir, "mytool", "v1")
	real := realPathOf(t, tool)
	t.Setenv("PATH", dir)

	stale := trustPolicy(t, "mytool", map[string]any{
		"dirs":   []string{realPathOf(t, dir)},
		"hashes": map[string]string{real: sha256Of(t, tool)},
	})
	fakeTool(t, dir, "mytool", "v2 — a legitimate upgrade")
	require.Equal(t, hitlservice.ActionApprove, evalTrust(t, stale, map[string]any{"command": "mytool"}).Action)

	// The refresh path: re-read the binary, rewrite the declaration.
	refreshedPath, refreshedSum, err := hitlservice.ResolveTrustedBinary("mytool")
	require.NoError(t, err)
	assert.Equal(t, real, refreshedPath, "the refresh must resolve to the same real path the evaluator does")
	fresh := trustPolicy(t, "mytool", map[string]any{
		"dirs":   []string{realPathOf(t, dir)},
		"hashes": map[string]string{refreshedPath: refreshedSum},
	})
	assert.Equal(t, hitlservice.ActionAllow, evalTrust(t, fresh, map[string]any{"command": "mytool"}).Action,
		"a re-declared hash must restore the allow")
}

// TestUnit_TrustedBinaries_StrictPin pins that declaring any hash makes the
// pin strict: an undeclared binary is refused rather than waved through.
func TestUnit_TrustedBinaries_StrictPin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	declaredTool := fakeTool(t, dir, "mytool", "declared")
	fakeTool(t, dir, "othertool", "undeclared")
	t.Setenv("PATH", dir)

	svc := trustPolicy(t, "mytool,othertool", map[string]any{
		"dirs":   []string{realPathOf(t, dir)},
		"hashes": map[string]string{realPathOf(t, declaredTool): sha256Of(t, declaredTool)},
	})

	assert.Equal(t, hitlservice.ActionAllow, evalTrust(t, svc, map[string]any{"command": "mytool"}).Action)
	r := evalTrust(t, svc, map[string]any{"command": "othertool"})
	assert.Equal(t, hitlservice.ActionApprove, r.Action, "declared or refuse — there is no record-on-first-use mode")
	assert.Contains(t, r.Detail, "has no declared hash")
	assert.Contains(t, r.Detail, "contenox hitl trust", "the refusal must name the verb that fixes it")
}

// TestUnit_TrustedBinaries_DirsOnlyIsIdentityOnly pins that the two halves are
// independent: declaring dirs alone enforces identity without demanding hashes.
func TestUnit_TrustedBinaries_DirsOnlyIsIdentityOnly(t *testing.T) {
	root := t.TempDir()
	trustedDir := filepath.Join(root, "trusted")
	evilDir := filepath.Join(root, "evil")
	fakeTool(t, trustedDir, "mytool", "the real one")
	fakeTool(t, evilDir, "mytool", "PWNED")

	svc := trustPolicy(t, "mytool", map[string]any{"dirs": []string{realPathOf(t, trustedDir)}})

	t.Setenv("PATH", trustedDir)
	assert.Equal(t, hitlservice.ActionAllow, evalTrust(t, svc, map[string]any{"command": "mytool"}).Action,
		"dirs alone must not demand a declared hash")
	t.Setenv("PATH", evilDir+string(os.PathListSeparator)+trustedDir)
	assert.Equal(t, hitlservice.ActionApprove, evalTrust(t, svc, map[string]any{"command": "mytool"}).Action)
}

// TestUnit_TrustedBinaries_UnresolvableAndRelativeAreRefused pins that failing
// to answer "which binary" is a refusal, never a pass.
func TestUnit_TrustedBinaries_UnresolvableAndRelativeAreRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	tool := fakeTool(t, dir, "mytool", "real")
	t.Setenv("PATH", dir)

	svc := trustPolicy(t, "mytool,./mytool,nosuchtool", map[string]any{
		"dirs":   []string{realPathOf(t, dir)},
		"hashes": map[string]string{realPathOf(t, tool): sha256Of(t, tool)},
	})

	r := evalTrust(t, svc, map[string]any{"command": "nosuchtool"})
	assert.Equal(t, hitlservice.ActionApprove, r.Action)
	assert.Contains(t, r.Detail, "does not resolve to an executable")

	rel := evalTrust(t, svc, map[string]any{"command": "./mytool"})
	assert.Equal(t, hitlservice.ActionApprove, rel.Action, "a cwd-relative path is not resolvable at policy time")
	assert.Contains(t, rel.Detail, "relative path")
}

// TestUnit_TrustedBinaries_AbsentBlockChangesNothing is the monotonicity
// anchor: every policy written before this existed answers exactly as before.
func TestUnit_TrustedBinaries_AbsentBlockChangesNothing(t *testing.T) {
	svc := trustPolicy(t, "git status,go build", nil)
	for _, args := range []map[string]any{
		{"command": "git", "args": []any{"status"}},
		{"command": "git status && go build"},
		{"command": "go build ./..."},
	} {
		assert.Equalf(t, hitlservice.ActionAllow, evalTrust(t, svc, args).Action,
			"%v must answer exactly as it did before trusted_binaries existed", args)
	}
}

// TestUnit_TrustedBinaries_NeverUpgradesAnAsk pins the monotonic contract from
// the other side: declarations cannot turn an ask or a deny into an allow.
func TestUnit_TrustedBinaries_NeverUpgradesAnAsk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	tool := fakeTool(t, dir, "mytool", "real")
	t.Setenv("PATH", dir)
	trusted := map[string]any{
		"dirs":   []string{realPathOf(t, dir)},
		"hashes": map[string]string{realPathOf(t, tool): sha256Of(t, tool)},
	}

	// mytool is fully declared but NOT on the prefix list: still an ask.
	svc := trustPolicy(t, "go build", trusted)
	assert.Equal(t, hitlservice.ActionApprove, evalTrust(t, svc, map[string]any{"command": "mytool"}).Action,
		"a declared binary that no rule allows must stay an ask")
}

// TestUnit_TrustedBinaries_GateOnlyWithdrawsAllows is the monotonicity gate's
// sharpest edge: command_prefix_allowlist is named for allow rules but nothing
// stops a policy pairing it with deny or approve. Withdrawing the match there
// would let the call THROUGH — the one shape this layer may never have — so
// the binary check applies to allow rules only.
func TestUnit_TrustedBinaries_GateOnlyWithdrawsAllows(t *testing.T) {
	root := t.TempDir()
	trustedDir := filepath.Join(root, "trusted")
	evilDir := filepath.Join(root, "evil")
	good := fakeTool(t, trustedDir, "mytool", "real")
	fakeTool(t, evilDir, "mytool", "PWNED")
	// The untrusted one is what resolves.
	t.Setenv("PATH", evilDir+string(os.PathListSeparator)+trustedDir)

	trusted, err := json.Marshal(map[string]any{
		"dirs":   []string{realPathOf(t, trustedDir)},
		"hashes": map[string]string{realPathOf(t, good): sha256Of(t, good)},
	})
	require.NoError(t, err)

	for action, want := range map[string]hitlservice.Action{
		"deny":    hitlservice.ActionDeny,
		"approve": hitlservice.ActionApprove,
	} {
		dir := t.TempDir()
		writePolicy(t, dir, "hitl-policy.json", []byte(`{"default_action":"allow","rules":[
			{"tools":"local_shell","tool":"local_shell","action":"`+action+`","when":[
				{"key":"command","op":"command_prefix_allowlist","value":"mytool"}]}
		],"trusted_binaries":`+string(trusted)+`}`))
		svc := hitlservice.New(hitlservice.NewFSPolicySource(dir), testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})
		assert.Equalf(t, want, evalTrust(t, svc, map[string]any{"command": "mytool"}).Action,
			"an untrusted binary must not escape a %q rule by failing the binary check", action)
	}
}

// TestUnit_TrustedBinaries_MalformedBlockFailsClosed pins that a typo in the
// enforcement block fails the policy to load — and the fallback is the
// rule-free approve-everything default, not the policy minus its gate.
func TestUnit_TrustedBinaries_MalformedBlockFailsClosed(t *testing.T) {
	for name, block := range map[string]string{
		"unknown field":      `{"dirs":["/usr/bin"],"hash":{}}`,
		"relative dir":       `{"dirs":["usr/bin"]}`,
		"relative hash key":  `{"hashes":{"bin/git":"` + strings.Repeat("a", 64) + `"}}`,
		"short digest":       `{"hashes":{"/usr/bin/git":"abc123"}}`,
		"non-hex digest":     `{"hashes":{"/usr/bin/git":"` + strings.Repeat("z", 64) + `"}}`,
		"digest not a value": `{"hashes":{"/usr/bin/git":""}}`,
	} {
		dir := t.TempDir()
		writePolicy(t, dir, "hitl-policy.json", []byte(`{"default_action":"allow","rules":[
			{"tools":"local_shell","tool":"local_shell","action":"allow"}],"trusted_binaries":`+block+`}`))
		svc := hitlservice.New(hitlservice.NewFSPolicySource(dir), testTenant, fixedKVReader{"hitl-policy.json"}, libtracker.NoopTracker{})
		r := evalTrust(t, svc, map[string]any{"command": "anything"})
		assert.Equalf(t, hitlservice.ActionApprove, r.Action,
			"%s: a malformed enforcement block must fail the policy closed, not load it disarmed", name)
	}
}

// TestUnit_TrustedBinaries_VetRejectsMalformedDeclarations pins the
// authoring-time half: the same defects are reported before anything runs.
//
// The absolute paths are built per platform on purpose. "/usr/bin" is NOT
// absolute on Windows (verified: filepath.IsAbs is false without a volume),
// so a declaration block carrying another platform's paths fails validation
// there — which is correct, and is why declarations are host-specific.
func TestUnit_TrustedBinaries_VetRejectsMalformedDeclarations(t *testing.T) {
	base := `{"default_action":"approve","rules":[],"trusted_binaries":`
	absDir := "/usr/bin"
	absBin := "/usr/bin/git"
	relDir := "../bin"
	if runtime.GOOS == "windows" {
		absDir = `C:\\Program Files\\Git\\cmd`
		absBin = `C:\\Program Files\\Git\\cmd\\git.exe`
		relDir = `..\\bin`
	}
	for name, block := range map[string]string{
		"unknown field": `{"dirs":["` + absDir + `"],"hashez":{}}`,
		"relative dir":  `{"dirs":["` + relDir + `"]}`,
		"short digest":  `{"hashes":{"` + absBin + `":"deadbeef"}}`,
	} {
		assert.Errorf(t, hitlservice.VetPolicy([]byte(base+block+`}`)), "%s must fail vet", name)
	}
	valid := base + `{"dirs":["` + absDir + `"],"hashes":{"` + absBin + `":"` + strings.Repeat("a", 64) + `"}}}`
	assert.NoError(t, hitlservice.VetPolicy([]byte(valid)))
}

// TestUnit_TrustedBinaries_CheckReportsHostState pins what vet and doctor show
// an operator for declarations that no longer describe this host.
func TestUnit_TrustedBinaries_CheckReportsHostState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	tool := fakeTool(t, dir, "mytool", "v1")
	real := realPathOf(t, tool)

	ok := &hitlservice.TrustedBinaries{Hashes: map[string]string{real: sha256Of(t, tool)}}
	assert.Empty(t, hitlservice.CheckTrustedBinaries(ok), "a matching declaration reports nothing")

	fakeTool(t, dir, "mytool", "v2")
	mismatch := hitlservice.CheckTrustedBinaries(ok)
	require.Len(t, mismatch, 1)
	assert.Equal(t, hitlservice.TrustedBinaryMismatch, mismatch[0].State)
	assert.Contains(t, mismatch[0].String(), "re-declare after verifying the upgrade, or investigate")

	missing := hitlservice.CheckTrustedBinaries(&hitlservice.TrustedBinaries{
		Hashes: map[string]string{filepath.Join(dir, "gone"): strings.Repeat("a", 64)},
	})
	require.Len(t, missing, 1)
	assert.Equal(t, hitlservice.TrustedBinaryMissing, missing[0].State)
}
