//go:build linux

package libsandbox_test

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/libsandbox"
	"github.com/stretchr/testify/require"
)

// TestIntegration_DeviceFloor proves the /dev character-device floor grants a
// confined process write to /dev/null and read to /dev/zero/urandom — the
// nodes every POSIX toolchain assumes exist. The complementary guarantee (that
// this grant does not reopen device-node creation) is asserted separately at
// the bitmask level by TestUnit_llWrite_NoDeviceNodeCreation.
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
