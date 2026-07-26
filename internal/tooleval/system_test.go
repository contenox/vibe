package tooleval_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/internal/tooleval"
)

// This is the LIVE-model half of the seam: the same runner and the same real tools as
// the hermetic self-tests, but with the Model seam bound to a configured provider
// (engineModel). It is gated and opt-in — CI and default `go test` runs stay hermetic,
// exactly like modelrepo's CONTENOX_RUN_VLLM_TESTS. A run's results land in a
// timestamped JSON file.
//
// Determinism honesty (docs/development/blueprints/tool-hardening.md):
// these are MEASUREMENTS over a stochastic system, not
// assertions about one. Seed and temperature are pinned where the provider honors them
// and recorded in every result; N=1 by default with a repeat knob. The test therefore
// records and reports — it does NOT fail on a model's task outcome or malformed rate
// (that is the matrix's job to expose, cell by cell, not a build gate's).
const (
	runToolEvalsEnv = "CONTENOX_RUN_TOOL_EVALS"
	modelEnv        = "CONTENOX_TOOL_EVAL_MODEL"
	providerEnv     = "CONTENOX_TOOL_EVAL_PROVIDER"
	urlEnv          = "CONTENOX_TOOL_EVAL_URL"
	repeatsEnv      = "CONTENOX_TOOL_EVAL_REPEATS"
	outDirEnv       = "CONTENOX_TOOL_EVAL_OUT"

	defaultEvalModel    = "qwen2.5:0.5b"
	defaultEvalProvider = "ollama"
)

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func requireToolEvals(t *testing.T) {
	t.Helper()
	if envTruthy(os.Getenv(runToolEvalsEnv)) {
		return
	}
	t.Skipf("skipping tool-eval matrix; set %s=1 to run. Needs a configured model reachable locally "+
		"(%s / %s, default %s / %s — e.g. the maintainer's ollama). Results land under %s (default bin/).",
		runToolEvalsEnv, modelEnv, providerEnv, defaultEvalModel, defaultEvalProvider, outDirEnv)
}

// buildRealModel stands up the engine-backed model and probes it. It Skips (never
// fails) when nothing is reachable, so the gate degrades cleanly on a box with no
// model up — the "probe; skip cleanly otherwise" contract.
func buildRealModel(ctx context.Context, t *testing.T) (*tooleval.EngineModel, int, float64) {
	t.Helper()
	model := envOr(modelEnv, defaultEvalModel)
	provider := envOr(providerEnv, defaultEvalProvider)

	dbPath := filepath.Join(t.TempDir(), "tooleval.db")
	db, err := libdbexec.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	seed := 7
	temp := 0.0
	em, err := tooleval.NewEngineModel(ctx, db, tooleval.EngineModelConfig{
		Model:       model,
		Provider:    provider,
		BaseURL:     envOr(urlEnv, ""),
		Seed:        &seed,
		Temperature: &temp,
	})
	if err != nil {
		t.Skipf("could not build engine for model %q via %q: %v", model, provider, err)
	}
	t.Cleanup(em.Close)
	if err := em.Probe(ctx); err != nil {
		t.Skipf("no reachable model %q via %q (%v); start the backend and pull the model, then rerun", model, provider, err)
	}
	return em, seed, temp
}

func hasTag(s *tooleval.Scenario, tag string) bool {
	for _, tg := range s.Meta.Tags {
		if tg == tag {
			return true
		}
	}
	return false
}

// TestSystem_ToolEval_Matrix runs every scenario against the configured real model and
// emits the model × scenario matrix (malformed-rate its own column) plus the guidance
// A/B deltas. Required scenarios are asserted to have RUN and produced a result; their
// task outcome is reported, not gated.
func TestSystem_ToolEval_Matrix(t *testing.T) {
	requireToolEvals(t)
	ctx := context.Background()

	em, seed, temp := buildRealModel(ctx, t)

	scenarios, err := tooleval.LoadAll()
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}
	if len(scenarios) == 0 {
		t.Fatal("no scenarios found")
	}

	repeats := 1
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(repeatsEnv))); err == nil && v > 0 {
		repeats = v
	}

	var runs []*tooleval.RunResult
	var abPairs [][2]*tooleval.RunResult
	ranRequired := 0

	for _, s := range scenarios {
		base := tooleval.RunOptions{Seed: &seed, Temperature: &temp}
		if hasTag(s, "ab") {
			// Guidance A/B: one navigation-heavy fixture, run twice (off/on). The delta
			// is the deliverable; nothing here is asserted.
			offOpts := base
			offOpts.GuidanceOn = false
			offOpts.RunID = "off"
			off, err := tooleval.Run(ctx, s, em, t.TempDir(), offOpts)
			if err != nil {
				t.Fatalf("scenario %s (guidance off): %v", s.ID, err)
			}
			onOpts := base
			onOpts.GuidanceOn = true
			onOpts.RunID = "on"
			on, err := tooleval.Run(ctx, s, em, t.TempDir(), onOpts)
			if err != nil {
				t.Fatalf("scenario %s (guidance on): %v", s.ID, err)
			}
			runs = append(runs, off, on)
			abPairs = append(abPairs, [2]*tooleval.RunResult{off, on})
			continue
		}
		for i := 0; i < repeats; i++ {
			opts := base
			opts.RunID = strconv.Itoa(i)
			r, err := tooleval.Run(ctx, s, em, t.TempDir(), opts)
			if err != nil {
				t.Fatalf("scenario %s (run %d): %v", s.ID, i, err)
			}
			runs = append(runs, r)
			if s.Meta.Required {
				ranRequired++
			}
		}
	}

	rep := tooleval.NewReport(runs, abPairs,
		"measurements over a stochastic system; seed/temperature pinned where honored; N="+strconv.Itoa(repeats),
		"green cell = evidence, not proof; a red cell (model × scenario) is the signal (tool-hardening.md rec 10)")

	outDir := envOr(outDirEnv, "bin")
	path, err := rep.WriteToFile(outDir)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}

	var mtx bytes.Buffer
	rep.WriteMatrix(&mtx)
	t.Logf("tool-eval report written to %s\n\n%s", path, mtx.String())
	// Also to stdout so a plain `go test` run (no -v) still surfaces the matrix.
	rep.WriteMatrix(os.Stdout)

	if ranRequired == 0 {
		t.Errorf("no required scenarios ran; check scenarios/*/meta.json")
	}
}
