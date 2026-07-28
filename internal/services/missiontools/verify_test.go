package missiontools_test

// Tests for the conclusion verification gate (verify.go), driven through the
// real Exec path against the real sqlite-backed mission store.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/stretchr/testify/require"
)

// resultCall builds a model-shape mission_report input claiming kind=result
// with the given refs.
func resultInput(summary string, refs []string) (any, *taskengine.ToolsCall) {
	refsAny := make([]any, len(refs))
	for i, r := range refs {
		refsAny[i] = r
	}
	input := map[string]any{"kind": "result", "summary": summary, "refs": refsAny}
	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameReport}
	return input, call
}

func storedReport(t *testing.T, svc missionservice.Service, missionID string) *missionservice.Report {
	t.Helper()
	reports, err := svc.ListReports(t.Context(), missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	return reports[0]
}

// TestUnit_Verify_MissingArtifactDowngradesResult pins the gate's core: a positively missing artifact downgrades result to progress.
func TestUnit_Verify_MissingArtifactDowngradesResult(t *testing.T) {
	ctx, svc, missionID := setup(t)

	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "exists.txt"), []byte("real"), 0o644))

	downgrades := 0
	tools := missiontools.New(svc, nil, missiontools.WithDowngradeRecorder(func() { downgrades++ }))

	toolCtx := missiontools.WithWorkdir(missiontools.WithMissionID(ctx, missionID), workdir)
	input, call := resultInput("done, see files", []string{"exists.txt", "missing.txt"})
	out, _, err := tools.Exec(toolCtx, time.Now(), input, false, call)
	require.NoError(t, err, "the gate annotates; it never fails the call")
	require.Contains(t, out, "downgraded from result", "the tool's reply teaches the unit what happened")
	require.Contains(t, out, "missing.txt")

	rep := storedReport(t, svc, missionID)
	require.Equal(t, missionservice.ReportKindProgress, rep.Kind, "success downgraded to partial (progress)")
	require.Equal(t, "done, see files", rep.Summary, "the unit's own words are untouched")
	require.Equal(t, []string{"exists.txt", "missing.txt"}, rep.Refs, "nothing is discarded — every claimed ref is preserved")
	require.Contains(t, rep.Detail, "claimed artifacts not found", "the warning carries its greppable lead")
	require.Contains(t, rep.Detail, `"missing.txt"`, "the warning names exactly what is missing")
	require.NotContains(t, rep.Detail, `"exists.txt"`, "a present artifact is not accused")
	require.Equal(t, 1, downgrades, "the telemetry hook fires once per downgraded report")
}

// TestUnit_Verify_AllPresentStaysResult pins that a result whose artifacts all exist lands exactly as filed.
func TestUnit_Verify_AllPresentStaysResult(t *testing.T) {
	ctx, svc, missionID := setup(t)

	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "report.md"), []byte("real"), 0o644))

	downgrades := 0
	tools := missiontools.New(svc, nil, missiontools.WithDowngradeRecorder(func() { downgrades++ }))

	toolCtx := missiontools.WithWorkdir(missiontools.WithMissionID(ctx, missionID), workdir)
	input, call := resultInput("done", []string{"report.md"})
	out, _, err := tools.Exec(toolCtx, time.Now(), input, false, call)
	require.NoError(t, err)
	require.NotContains(t, out, "downgraded")

	rep := storedReport(t, svc, missionID)
	require.Equal(t, missionservice.ReportKindResult, rep.Kind)
	require.Empty(t, rep.Detail)
	require.Zero(t, downgrades)
}

// TestUnit_Verify_UnverifiableRefsFailOpen pins that URLs, prose, and relative paths with no workdir all count as present.
func TestUnit_Verify_UnverifiableRefsFailOpen(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	// No WithWorkdir: the relative ref is unverifiable by construction.
	toolCtx := missiontools.WithMissionID(ctx, missionID)
	input, call := resultInput("done, see the links", []string{
		"https://example.com/build/artifact.tar.gz", // URL: not a local fact
		"see the PR description for details",        // prose: a description, not a path
		"relative/never-written.txt",                // relative, no workdir: nothing to resolve against
	})
	_, _, err := tools.Exec(toolCtx, time.Now(), input, false, call)
	require.NoError(t, err)

	rep := storedReport(t, svc, missionID)
	require.Equal(t, missionservice.ReportKindResult, rep.Kind, "unverifiable refs count as present — fail-open")
	require.Empty(t, rep.Detail)
}

// TestUnit_Verify_AbsoluteMissingPathNeedsNoWorkdir pins that an absolute missing path downgrades even with no workdir bound.
func TestUnit_Verify_AbsoluteMissingPathNeedsNoWorkdir(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	gone := filepath.Join(t.TempDir(), "never-written.bin")
	toolCtx := missiontools.WithMissionID(ctx, missionID) // no workdir
	input, call := resultInput("done", []string{gone})
	_, _, err := tools.Exec(toolCtx, time.Now(), input, false, call)
	require.NoError(t, err)

	rep := storedReport(t, svc, missionID)
	require.Equal(t, missionservice.ReportKindProgress, rep.Kind)
	require.Contains(t, rep.Detail, gone)
}

// TestUnit_Verify_StatErrorCountsAsPresent pins that a non-not-exists stat error (e.g. a permission wall) never downgrades.
func TestUnit_Verify_StatErrorCountsAsPresent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission walls do not apply")
	}
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	workdir := t.TempDir()
	locked := filepath.Join(workdir, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "artifact.txt"), []byte("real"), 0o644))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	toolCtx := missiontools.WithWorkdir(missiontools.WithMissionID(ctx, missionID), workdir)
	input, call := resultInput("done", []string{"locked/artifact.txt"})
	_, _, err := tools.Exec(toolCtx, time.Now(), input, false, call)
	require.NoError(t, err)

	rep := storedReport(t, svc, missionID)
	require.Equal(t, missionservice.ReportKindResult, rep.Kind,
		"EACCES is not ENOENT: an unverifiable stat counts as present")
	require.Empty(t, rep.Detail)
}

// TestUnit_Verify_HandoverArtifactsAreClaimsToo pins that a missing hand-over artifact downgrades, and the hand-over still lands verbatim.
func TestUnit_Verify_HandoverArtifactsAreClaimsToo(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	workdir := t.TempDir()
	toolCtx := missiontools.WithWorkdir(missiontools.WithMissionID(ctx, missionID), workdir)
	input := map[string]any{
		"kind":    "result",
		"summary": "ported the module",
		"handover": map[string]any{
			"outcome":   "module ported",
			"artifacts": []any{"ported/module.go"},
		},
	}
	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameReport}
	_, _, err := tools.Exec(toolCtx, time.Now(), input, false, call)
	require.NoError(t, err)

	rep := storedReport(t, svc, missionID)
	require.Equal(t, missionservice.ReportKindProgress, rep.Kind)
	require.Contains(t, rep.Detail, `"ported/module.go"`)
	require.NotNil(t, rep.Handover, "the hand-over lands verbatim, downgrade or not")
	require.Equal(t, []string{"ported/module.go"}, rep.Handover.Artifacts)
	require.Equal(t, "module ported", rep.Handover.Outcome)
}

// TestUnit_Verify_OnlyResultsAreGated pins that a non-result report naming a missing path is never gated.
func TestUnit_Verify_OnlyResultsAreGated(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	workdir := t.TempDir()
	toolCtx := missiontools.WithWorkdir(missiontools.WithMissionID(ctx, missionID), workdir)
	input := map[string]any{"kind": "progress", "summary": "working on it", "refs": []any{"not-yet-written.txt"}}
	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameReport}
	_, _, err := tools.Exec(toolCtx, time.Now(), input, false, call)
	require.NoError(t, err)

	rep := storedReport(t, svc, missionID)
	require.Equal(t, missionservice.ReportKindProgress, rep.Kind)
	require.Empty(t, rep.Detail, "a non-result report is never annotated")
}

// TestUnit_Verify_WarningAppendsToExistingDetail pins that the unit's detail comes first, the runtime's warning after.
func TestUnit_Verify_WarningAppendsToExistingDetail(t *testing.T) {
	ctx, svc, missionID := setup(t)
	tools := missiontools.New(svc, nil)

	workdir := t.TempDir()
	toolCtx := missiontools.WithWorkdir(missiontools.WithMissionID(ctx, missionID), workdir)
	input := map[string]any{
		"kind": "result", "summary": "done",
		"detail": "wrote the migration and the rollback script",
		"refs":   []any{"migrations/0042.sql"},
	}
	call := &taskengine.ToolsCall{Name: missiontools.ToolsProviderName, ToolName: missiontools.ToolNameReport}
	_, _, err := tools.Exec(toolCtx, time.Now(), input, false, call)
	require.NoError(t, err)

	rep := storedReport(t, svc, missionID)
	require.Equal(t, missionservice.ReportKindProgress, rep.Kind)
	require.Contains(t, rep.Detail, "wrote the migration and the rollback script", "the unit's detail is preserved")
	require.Less(t,
		strings.Index(rep.Detail, "wrote the migration"),
		strings.Index(rep.Detail, "claimed artifacts not found"),
		"the unit's words come first, the runtime's note after")
}
