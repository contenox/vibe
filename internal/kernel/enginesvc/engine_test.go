package enginesvc_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

func hasIssueCode(issues []setupcheck.Issue, code string) bool {
	for _, iss := range issues {
		if iss.Code == code {
			return true
		}
	}
	return false
}

func issueCodes(issues []setupcheck.Issue) []string {
	out := make([]string, 0, len(issues))
	for _, iss := range issues {
		out = append(out, iss.Code)
	}
	return out
}

// TestSystem_Build_ReadinessDefaultModel_creditsExplicitFlag pins that a
// model supplied out-of-band via ReadinessDefaultModel (e.g. CLI --model)
// satisfies preflight even with no persisted default-model, in both the
// build-time snapshot and the live recompute.
func TestSystem_Build_ReadinessDefaultModel_creditsExplicitFlag(t *testing.T) {
	ctx := context.Background()
	newDB := func(t *testing.T) libdbexec.DBManager {
		t.Helper()
		path := filepath.Join(t.TempDir(), "b001.db")
		db, err := libdbexec.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	baseCfg := func() enginesvc.Config {
		return enginesvc.Config{
			DefaultModel:     "phi-4-mini",
			DefaultProvider:  "llama",
			SkipBackendCycle: true,
			NoDeleteModels:   true,
		}
	}

	t.Run("without readiness override the missing_default_model issue is present", func(t *testing.T) {
		eng, err := enginesvc.Build(ctx, newDB(t), baseCfg())
		if err != nil {
			t.Fatalf("build engine: %v", err)
		}
		defer eng.Stop()
		if !hasIssueCode(eng.SetupCheck.Issues, "missing_default_model") {
			t.Fatalf("baseline should report missing_default_model; issues=%v", issueCodes(eng.SetupCheck.Issues))
		}
	})

	t.Run("with readiness override the model is credited and the issue is gone", func(t *testing.T) {
		cfg := baseCfg()
		cfg.ReadinessDefaultModel = "phi-4-mini"
		eng, err := enginesvc.Build(ctx, newDB(t), cfg)
		if err != nil {
			t.Fatalf("build engine: %v", err)
		}
		defer eng.Stop()

		if hasIssueCode(eng.SetupCheck.Issues, "missing_default_model") {
			t.Errorf("snapshot still reports missing_default_model; issues=%v", issueCodes(eng.SetupCheck.Issues))
		}
		if eng.SetupCheck.DefaultModel != "phi-4-mini" {
			t.Errorf("snapshot DefaultModel = %q, want phi-4-mini", eng.SetupCheck.DefaultModel)
		}

		// The live recompute must apply the same overlay, not just the snapshot.
		live, err := eng.SetupStatus(ctx)
		if err != nil {
			t.Fatalf("live SetupStatus: %v", err)
		}
		if hasIssueCode(live.Issues, "missing_default_model") {
			t.Errorf("live recompute still reports missing_default_model; issues=%v", issueCodes(live.Issues))
		}
		if live.DefaultModel != "phi-4-mini" {
			t.Errorf("live DefaultModel = %q, want phi-4-mini", live.DefaultModel)
		}
	})
}
