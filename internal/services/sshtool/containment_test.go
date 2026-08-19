package sshtool_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/sshtool"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnit_Containment_PrivateKeyFileInControlPlaneIsRefused is the reach that
// matters: private_key_file is a call argument, so a model can point it anywhere.
// A path resolving into the runtime control plane is refused through the shared
// vfs seam before the file is opened and before the machine is dialled.
func TestUnit_Containment_PrivateKeyFileInControlPlaneIsRefused(t *testing.T) {
	srv := newRemote(t)

	cp := t.TempDir()
	keyPath := filepath.Join(cp, "id_ed25519")
	// mode 0600 so a refusal here is the control-plane deny, not the perm check.
	require.NoError(t, os.WriteFile(keyPath, []byte("not a key"), 0o600))

	prev := vfs.ControlPlaneDenied()
	defer vfs.SetControlPlaneDenied(prev...)
	require.NoError(t, vfs.SetControlPlaneDenied(cp))

	repo := newTools(t, srv.knownHosts(t))
	args := srv.callArgs("greet")
	delete(args, "password")
	args["private_key_file"] = keyPath

	before := len(srv.commands())
	_, _, err := execOn(withPolicy(map[string]string{"_allowed_hosts": srv.host}), repo, args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "control plane")
	assert.Contains(t, err.Error(), "fatal:", "a control-plane path is not fixable by retrying")
	assert.Len(t, srv.commands(), before, "a key read from the control plane still reached the machine")
}

// TestUnit_Containment_PrivateKeyFileEscapingFileRootIsRefused shows the operator
// ceiling: once WithFileRoot scopes local file reach, a private_key_file outside
// it is refused as an escape — the same containment local_fs enforces.
func TestUnit_Containment_PrivateKeyFileEscapingFileRootIsRefused(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)

	root := t.TempDir()
	knownHosts := filepath.Join(root, "known_hosts")
	require.NoError(t, os.WriteFile(knownHosts, nil, 0o600))
	repo := newTools(t, knownHosts, sshtool.WithFileRoot(root))

	// A key that lives outside the configured root.
	keyOutside := filepath.Join(t.TempDir(), "id_ed25519")
	require.NoError(t, os.WriteFile(keyOutside, []byte("not a key"), 0o600))

	args := srv.callArgs("greet")
	delete(args, "password")
	args["private_key_file"] = keyOutside

	before := len(srv.commands())
	_, _, err := execOn(withPolicy(map[string]string{"_allowed_hosts": srv.host}), repo, args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes")
	assert.Contains(t, err.Error(), "SSH file root", "the refusal does not name the file root that would fix it")
	assert.Contains(t, err.Error(), "fatal:")
	assert.Len(t, srv.commands(), before, "a key outside the file root still reached the machine")
}

// TestUnit_Containment_KnownHostsInControlPlaneIsRefused proves the containment
// covers the known_hosts read too, not only keys: a known_hosts file resolving
// into the control plane fails construction rather than being loaded.
func TestUnit_Containment_KnownHostsInControlPlaneIsRefused(t *testing.T) {
	cp := t.TempDir()
	knownHosts := filepath.Join(cp, "known_hosts")
	require.NoError(t, os.WriteFile(knownHosts, nil, 0o600))

	prev := vfs.ControlPlaneDenied()
	defer vfs.SetControlPlaneDenied(prev...)
	require.NoError(t, vfs.SetControlPlaneDenied(cp))

	_, err := sshtool.NewSSHTools(sshtool.WithKnownHostsFile(knownHosts))
	require.Error(t, err, "a known_hosts file inside the control plane was loaded")
	assert.Contains(t, err.Error(), "control plane")
}

// TestUnit_Containment_FileRootStillPermitsAContainedRead is the other side of
// the boundary: a key inside the configured root, not in the control plane, is
// contained rather than refused (it fails later, on its own contents/mode).
func TestUnit_Containment_FileRootStillPermitsAContainedRead(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)

	root := t.TempDir()
	knownHosts := filepath.Join(root, "known_hosts")
	require.NoError(t, os.WriteFile(knownHosts, nil, 0o600))
	repo := newTools(t, knownHosts, sshtool.WithFileRoot(root))

	keyInside := filepath.Join(root, "id_ed25519")
	require.NoError(t, os.WriteFile(keyInside, []byte("not a key"), 0o600))

	args := srv.callArgs("greet")
	delete(args, "password")
	args["private_key_file"] = keyInside

	_, _, err := execOn(withPolicy(map[string]string{"_allowed_hosts": srv.host}), repo, args)
	require.Error(t, err)
	// Containment let the read through; the failure is the key parse, not a refusal.
	assert.NotContains(t, err.Error(), "escapes")
	assert.NotContains(t, err.Error(), "control plane")
	assert.Contains(t, err.Error(), "private key")
}

// TestUnit_Containment_SupportsLeadsWithTheScopedName ties the allowlist to the
// name Supports reports: it leads with the native-scoped provider name, which
// "*" admits, "!name" removes and an exact grant names. A rename is answered the
// same way.
func TestUnit_Containment_SupportsLeadsWithTheScopedName(t *testing.T) {
	t.Parallel()

	supported, err := newTools(t, "").Supports(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, supported)

	provider := supported[0]
	assert.Equal(t, sshtool.ToolsProviderName, provider, "Supports() must lead with the registered toolset name")
	// native- is a namespace, so a declared MCP source cannot mint this key.
	require.Truef(t, strings.HasPrefix(provider, "native-"),
		"provider name %q dropped the native- namespace; a declared source could collide with it", provider)
	assert.Contains(t, supported, sshtool.ToolExecuteRemoteCommand, "the addressable tool must be reported alongside the toolset")

	universe := []string{provider}
	assert.Contains(t, taskengine.ExportedApplyAllowlist([]string{"*"}, universe), provider,
		"\"*\" must admit the scoped toolset; the scope is a namespace, not a hidden exclusion")
	assert.NotContains(t, taskengine.ExportedApplyAllowlist([]string{"*", "!" + provider}, universe), provider,
		"\"!\"+the toolset name must remove it from under the wildcard")
	assert.Contains(t, taskengine.ExportedApplyAllowlist([]string{provider}, universe), provider,
		"naming the toolset exactly does not admit it")
	assert.Empty(t, taskengine.ExportedApplyAllowlist(nil, universe),
		"an empty allowlist grants nothing")

	const alt = "native-ssh-alt"
	renamed, err := newTools(t, "", sshtool.WithName(alt)).Supports(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, renamed)
	assert.Equal(t, alt, renamed[0], "a renamed registration must answer to its scoped key everywhere")
}

// TestUnit_Containment_LaunchesNoLocalProcess encodes why the env-scrub has no
// call site here: this toolset reaches remote hosts over an in-process SSH client
// and spawns no local process, so there is no child environment to scrub. If it
// ever grows one, this guard fires: that process's env must be routed through
// libsandbox EnvScrub (resolvedSandboxEnv), never raw os.Environ().
func TestUnit_Containment_LaunchesNoLocalProcess(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		text := string(src)
		assert.NotContainsf(t, text, "os/exec",
			"%s launches a local process; route its env through libsandbox EnvScrub / resolvedSandboxEnv, never raw os.Environ()", f)
		assert.NotContainsf(t, text, "os.Environ",
			"%s reads the raw process environment; a spawned child must receive a scrubbed env instead", f)
	}
}
