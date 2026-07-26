//go:build linux

package libsandbox_test

import (
	"context"
	"testing"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/stretchr/testify/require"
)

// TestIntegration_DeviceFloor proves the /dev character-device floor has teeth: a
// confined process can WRITE /dev/null and READ the stateless read-only nodes
// (/dev/zero, /dev/urandom) — exactly the devices every POSIX toolchain assumes
// exist merely to run. Before the floor nothing under /dev was granted, so a
// confined Bash broke the moment it redirected to /dev/null.
//
// The complementary guarantee — that granting WRITE on /dev/null does NOT reopen
// device-node creation — is asserted at the bitmask level by
// TestUnit_llWrite_NoDeviceNodeCreation, which is the ATTRIBUTABLE way to prove it:
// a behavioural mknod probe would be refused by the user-namespace device policy
// (or by lacking CAP_MKNOD) rather than by Landlock, so its denial could not be
// pinned on the wall. This suite therefore asserts only the positive grants.
func TestIntegration_DeviceFloor(t *testing.T) {
	if !landlockSupported() {
		t.Skip("landlock filesystem ABI unavailable on this kernel")
	}
	if !usernsNetnsSupported() {
		t.Skip("unprivileged user+network namespaces unavailable on this host")
	}

	ws := t.TempDir()
	home := t.TempDir()

	newSpec := func(action, path string) libsandbox.Spec {
		return libsandbox.Spec{
			WorkspaceRoot: ws,
			Home:          home,
			EnvSet: map[string]string{
				probeEnv:     action,
				probePathEnv: path,
			},
		}
	}

	cases := []struct {
		name   string
		action string
		path   string
		want   int
	}{
		{"write to /dev/null is allowed", "write", "/dev/null", exitAllowed},
		{"read /dev/zero is allowed", "read", "/dev/zero", exitAllowed},
		{"read /dev/urandom is allowed", "read", "/dev/urandom", exitAllowed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := libsandbox.Command(context.Background(), newSpec(tc.action, tc.path), "/proc/self/exe")
			require.NoError(t, err)
			require.Equal(t, tc.want, exitCode(cmd.Run()),
				"device probe %q on %q: unexpected outcome", tc.action, tc.path)
		})
	}
}
