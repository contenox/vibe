package gointel

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/vfs"
)

// lastWinsEnv indexes KEY=VALUE entries the way exec.Cmd's dedup does: the last
// value for a key wins, so an appended override is what the launched process sees.
func lastWinsEnv(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			out[kv[:eq]] = kv[eq+1:]
		}
	}
	return out
}

// TestSystem_GoIntel_Containment_ControlPlaneDirIsRefused pins that a query dir
// resolving into the runtime's own control plane is refused through vfs, carrying
// vfs.ErrControlPlane, and is never indexed — even when that dir is itself a
// loadable module. The containment seam is internal/services/vfs, the same one
// local_fs uses; gointel reaches through it, it does not re-implement it.
func TestSystem_GoIntel_Containment_ControlPlaneDirIsRefused(t *testing.T) {
	root := newFixture(t, "fixture")

	cpDir := filepath.Join(root, "control_plane")
	writeModule(t, cpDir, "example.com/controlplane", map[string]string{
		"state.go": "package controlplane\n\n// DBPassword must never be reachable through the index.\nconst DBPassword = \"leaked\"\n",
	})

	// The denylist is process-global; restore whatever was registered before.
	saved := vfs.ControlPlaneDenied()
	t.Cleanup(func() { _ = vfs.SetControlPlaneDenied(saved...) })
	if err := vfs.SetControlPlaneDenied(cpDir); err != nil {
		t.Fatalf("SetControlPlaneDenied: %v", err)
	}

	repo := NewTools(newTestIndex(t, root))

	for _, dir := range []string{"control_plane", "control_plane/", "./control_plane"} {
		out, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "shapes.Rect", "dir": dir})
		if err == nil {
			t.Errorf("dir %q was ACCEPTED (result %#v) — the control plane is never a workspace", dir, out)
			continue
		}
		if !errors.Is(err, vfs.ErrControlPlane) {
			t.Errorf("dir %q: error %v is not vfs.ErrControlPlane — a caller cannot tell a control-plane refusal from any other", dir, err)
		}
		if !strings.Contains(err.Error(), "control plane") {
			t.Errorf("dir %q: error %q does not name the control plane as the cause", dir, err)
		}
		assertTeachingError(t, "control_plane/"+dir, err)
	}

	// The constant that only exists inside the control-plane module must not be
	// reachable from the workspace by any spelling either.
	for _, symbol := range []string{"controlplane.DBPassword", "DBPassword", "example.com/controlplane.DBPassword"} {
		if _, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": symbol}); err == nil {
			t.Errorf("symbol %q resolved into the control-plane module", symbol)
		}
	}
}

// TestSystem_GoIntel_Containment_EscapingDirIsRefused reaffirms, in the same
// containment suite, that a dir escaping the workspace root is refused with
// ErrOutsideAllowedDir — the exhaustive spellings live in the hostile-dir test.
func TestSystem_GoIntel_Containment_EscapingDirIsRefused(t *testing.T) {
	root := newFixture(t, "fixture")
	repo := NewTools(newTestIndex(t, root))

	for _, dir := range []string{"../..", realSystemDir(t)} {
		out, err := execTool(t, repo, ToolDefinition, map[string]any{"symbol": "shapes.Rect", "dir": dir})
		if err == nil {
			t.Errorf("dir %q was ACCEPTED (result %#v) — containment is the whole boundary", dir, out)
			continue
		}
		if !errors.Is(err, ErrOutsideAllowedDir) {
			t.Errorf("dir %q: error %v is not ErrOutsideAllowedDir", dir, err)
		}
	}
}

// TestUnit_GoIntel_Containment_GoInvocationEnvIsScrubbed pins that the env every
// `go` subprocess this package launches runs under is scrubbed off the raw
// os.Environ() through libsandbox: control-plane and credential-shaped vars are
// stripped, the ordinary vars the toolchain needs survive, and GOTOOLCHAIN is
// pinned to local so a workspace go.mod cannot pull a toolchain over the network.
func TestUnit_GoIntel_Containment_GoInvocationEnvIsScrubbed(t *testing.T) {
	t.Setenv("CONTENOX_DATABASE_URL", "postgres://control-plane")
	t.Setenv("GITHUB_TOKEN", "ghp_must_not_leak")
	t.Setenv("MY_PASSWORD", "hunter2")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws_must_not_leak")
	t.Setenv("GOFLAGS", "-mod=mod") // an ordinary go var the toolchain needs
	t.Setenv("GOTOOLCHAIN", "auto") // the parent tries auto; local must win

	env := lastWinsEnv(goInvocationEnv())

	for _, secret := range []string{"CONTENOX_DATABASE_URL", "GITHUB_TOKEN", "MY_PASSWORD", "AWS_SECRET_ACCESS_KEY"} {
		if v, ok := env[secret]; ok {
			t.Errorf("%s=%q survived the scrub; a launched go process must never inherit it", secret, v)
		}
	}
	if env["GOFLAGS"] != "-mod=mod" {
		t.Errorf("GOFLAGS = %q, want it preserved — the toolchain needs its ordinary environment", env["GOFLAGS"])
	}
	if env["GOTOOLCHAIN"] != "local" {
		t.Errorf("GOTOOLCHAIN = %q, want local — a workspace go.mod must not trigger a toolchain download", env["GOTOOLCHAIN"])
	}
}
