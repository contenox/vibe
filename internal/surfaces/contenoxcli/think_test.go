package contenoxcli

import (
	"encoding/json"
	"testing"

	"github.com/contenox/beam/internal/services/clikv"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func newThinkFlagSet(t *testing.T, value string) *pflag.FlagSet {
	t.Helper()
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("think", "", "")
	if value != "" {
		require.NoError(t, flags.Set("think", value))
	}
	return flags
}

func TestUnit_ResolveEffectiveThink_DefaultConfigAndFlagPrecedence(t *testing.T) {
	ctx, _, store := openTestDB(t)

	got, err := resolveEffectiveThink(ctx, store, newThinkFlagSet(t, ""))
	require.NoError(t, err)
	require.Equal(t, "high", got)

	data, err := json.Marshal("medium")
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, clikv.Prefix+"default-think", data))
	got, err = resolveEffectiveThink(ctx, store, newThinkFlagSet(t, ""))
	require.NoError(t, err)
	require.Equal(t, "medium", got)

	got, err = resolveEffectiveThink(ctx, store, newThinkFlagSet(t, "off"))
	require.NoError(t, err)
	require.Equal(t, "off", got)
}

func TestUnit_ResolveEffectiveThink_InvalidFlagAndConfigError(t *testing.T) {
	ctx, _, store := openTestDB(t)
	_, err := resolveEffectiveThink(ctx, store, newThinkFlagSet(t, "bogus"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "--think")

	data, err := json.Marshal("bogus")
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, clikv.Prefix+"default-think", data))
	_, err = resolveEffectiveThink(ctx, store, newThinkFlagSet(t, ""))
	require.Error(t, err)
	require.Contains(t, err.Error(), "config default-think")
}

func TestUnit_FirstNonFlagIsReserved_ThinkConsumesValue(t *testing.T) {
	require.True(t, firstNonFlagIsReserved([]string{"--think", "off", "chat"}))
	require.True(t, firstNonFlagIsReserved([]string{"--think=off", "chat"}))
}

func TestUnit_ShouldPrintThinking(t *testing.T) {
	require.False(t, shouldPrintThinking("off"))
	require.False(t, shouldPrintThinking("auto"))
	require.True(t, shouldPrintThinking("minimal"))
	require.True(t, shouldPrintThinking("high"))
}
