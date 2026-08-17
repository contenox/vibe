package contenoxcli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/substrate"
	"github.com/stretchr/testify/require"
)

func unsetSubstrateEnv(t *testing.T) {
	t.Helper()
	t.Setenv(substrate.PostgresURLEnv, "")
	t.Setenv(substrate.NATSURLEnv, "")
	t.Setenv(substrate.ValkeyURLEnv, "")
}

func TestUnit_Init_StopsWhenASettingNamesAStoreItCannotUse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetSubstrateEnv(t)
	t.Setenv(substrate.PostgresURLEnv, "postgres://contenox:topsecret@db:5432/contenox")

	var out, errOut bytes.Buffer
	err := RunInit(&out, &errOut, false, false, "openai", filepath.Join(home, "ws", ".contenox"), "")
	require.Error(t, err)
	require.Contains(t, err.Error(), substrate.PostgresURLEnv, "the setting that stopped it must be named")
	require.NotContains(t, err.Error(), "topsecret")
	require.Empty(t, out.String(), "nothing may be reported before the store is known to work")
	require.Empty(t, errOut.String())
}

func TestUnit_Init_StopsWhenTheStoreItWasToldToUseIsUnreachable(t *testing.T) {
	if testing.Short() {
		t.Skip("dials a port that must be closed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetSubstrateEnv(t)
	t.Setenv(substrate.PostgresURLEnv, "postgres://contenox:topsecret@127.0.0.1:15432/contenox?sslmode=disable")
	t.Setenv(substrate.NATSURLEnv, "nats://127.0.0.1:14222")
	t.Setenv(substrate.ValkeyURLEnv, "valkey://127.0.0.1:16379")

	var out, errOut bytes.Buffer
	err := RunInit(&out, &errOut, false, false, "openai", filepath.Join(home, "ws", ".contenox"), "")
	require.Error(t, err)
	require.Contains(t, err.Error(), substrate.PostgresURLEnv)
	require.NotContains(t, err.Error(), "topsecret")
	require.Empty(t, out.String())
}

func TestUnit_Init_StillFinishesWhenNothingNamesAStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetSubstrateEnv(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".contenox", "local.db"), 0o750))

	var out, errOut bytes.Buffer
	require.NoError(t, RunInit(&out, &errOut, false, false, "openai", filepath.Join(home, "ws", ".contenox"), ""))
	require.Contains(t, out.String(), "Done.")
	require.NotContains(t, out.String(), "Current config")
}

func TestUnit_Setup_StopsWhenASettingNamesAStoreItCannotUse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetSubstrateEnv(t)
	t.Setenv(substrate.PostgresURLEnv, "postgres://contenox:topsecret@db:5432/contenox")

	var out bytes.Buffer
	err := runSetup(testCobraCmd(), &out)
	require.Error(t, err)
	require.Contains(t, err.Error(), substrate.PostgresURLEnv)
	require.NotContains(t, err.Error(), "topsecret")
	require.NotContains(t, out.String(), "Choose your LLM provider",
		"a wizard whose answers cannot be saved must not be offered")
}
