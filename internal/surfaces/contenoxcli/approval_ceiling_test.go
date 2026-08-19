package contenoxcli

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func ceilingOf(t *testing.T, svc hitlservice.Service) time.Duration {
	t.Helper()
	reader, ok := svc.(interface{ ApprovalCeiling() time.Duration })
	require.True(t, ok, "the service must be able to report the wait it applies")
	return reader.ApprovalCeiling()
}

// TestUnit_ApprovalCeiling_ConfigKeySetsTheFleetWideWait drives what an
// operator does: `contenox config set approval-ceiling <value>`, then any
// command that raises an ask. Unset is the compiled-in fallback; "never" is
// the wait that has no deadline at all.
func TestUnit_ApprovalCeiling_ConfigKeySetsTheFleetWideWait(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "unset", want: hitlservice.FallbackApprovalCeiling},
		{name: "a duration", value: "24h", want: 24 * time.Hour},
		{name: "minutes", value: "90m", want: 90 * time.Minute},
		{name: "never", value: "never", want: hitlservice.WaitIndefinite},
		{name: "forever", value: "forever", want: hitlservice.WaitIndefinite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			contenoxDir := t.TempDir()
			db, err := libdb.NewSQLiteDBManager(ctx, t.TempDir()+"/ceiling.db", runtimetypes.SchemaSQLite)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			store := runtimetypes.New(db.WithoutTransaction())

			if tc.value != "" {
				// What `contenox config set approval-ceiling <value>` persists.
				normalized, err := normalizeApprovalCeiling(approvalCeilingKey, tc.value)
				require.NoError(t, err)
				require.NoError(t, clikv.SetString(ctx, store, approvalCeilingKey, normalized))
			}

			svc := newHITLService(ctx, contenoxDir, store, libtracker.NoopTracker{}, "")
			require.Equal(t, tc.want, ceilingOf(t, svc))
		})
	}
}

// TestUnit_NormalizeApprovalCeiling_RefusesWhatItCannotMean keeps the refusal
// in front of the person who typed it, and teachable: a near miss has to name
// the words that would have worked.
func TestUnit_NormalizeApprovalCeiling_RefusesWhatItCannotMean(t *testing.T) {
	for _, value := range []string{"", "0", "0s", "-30m", "half a day", "1500ms", "200h", "none"} {
		_, err := normalizeApprovalCeiling(approvalCeilingKey, value)
		require.Errorf(t, err, "approval-ceiling=%q must be refused", value)
		require.Contains(t, err.Error(), approvalCeilingKey)
	}

	_, err := normalizeApprovalCeiling(approvalCeilingKey, "banana")
	require.Error(t, err)
	require.Contains(t, err.Error(), hitlservice.IndefiniteSpellings())

	// Stored canonically, so `config get` reads back what the runtime applies.
	for value, want := range map[string]string{
		"30m": "30m0s", " NEVER ": "never", "24h": "24h0m0s", "indefinite": "never",
	} {
		got, err := normalizeApprovalCeiling(approvalCeilingKey, value)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	// A key this file does not own passes straight through.
	got, err := normalizeApprovalCeiling("default-model", "qwen3:8b")
	require.NoError(t, err)
	require.Empty(t, got)
}
