package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check LLM setup: defaults, registered backends, and connectivity.",
	Long: `Shows whether your default model and provider are set, lists every registered backend
(Ollama local or Cloud, OpenAI, Anthropic, Gemini, Vertex AI, AWS Bedrock, vLLM),
and reports reachability and setup
issues for each. Use it after contenox init, after contenox backend add, or when
chat/run cannot resolve a model.

Additionally, if you use local Ollama: when no Ollama backend is ready yet, doctor may probe
your Ollama URL (OLLAMA_HOST, or http://127.0.0.1:11434) and suggest commands to pull a model
(at least ollama pull qwen3:8b), register the backend, and set defaults—including --url for a
non-default host or port.

Examples:
  contenox doctor
  contenox doctor --json
  contenox doctor --skip-cycle
  contenox doctor --bundle`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().Bool("json", false, "Print results as JSON")
	doctorCmd.Flags().Bool("skip-cycle", false, "Skip syncing backends (faster; status may be outdated)")
	doctorCmd.Flags().Bool("bundle", false, "Also write a redacted diagnostics zip (this report as JSON, build info, logs) and print a pre-filled issue URL")
	doctorCmd.Flags().String("bundle-out", "", "Where --bundle writes (default: ./contenox-doctor-<timestamp>.zip)")
}

func runDoctor(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return err
	}
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	o, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		return err
	}
	o.EffectiveDB = dbPath
	o.EffectiveSkipBackendCycle, _ = cmd.Flags().GetBool("skip-cycle")

	// Built directly (instead of via ComputeReadiness) so the synced runtime
	// state is readable for the vision summary without a second backend cycle.
	engine, err := BuildEngine(ctx, db, o)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	res := setupcheck.EnrichResultWithOllamaProbe(ctx, engine.SetupCheck)
	// A policy predating a toolset is invisible from the inside (an approval
	// card per read, not a failure), so doctor names it. Beta-gated toolsets
	// are skipped: an invisible toolset cannot make an envelope stale.
	res = setupcheck.AddStalePolicyPresetIssue(res, stalePolicyPresetIssues(policyDirs(contenoxDir), betaGatedToolsets(o.EffectiveOptInBeta)), RefreshPoliciesCommand)
	// A trusted-binary declaration that stopped matching is equally invisible
	// from the inside: the allow is withdrawn and the call asks a human, with
	// no clue that a binary changed underneath it.
	res = setupcheck.AddTrustedBinaryIssue(res, trustedBinaryDrift(policyDirs(contenoxDir)), TrustBinariesRefreshCommand)
	vision := visionSummaryFromState(engine.State.Get(ctx), res.DefaultModel)
	engine.Stop()

	// Written to stderr on the --json path so stdout stays a single parseable
	// payload.
	bundleW := cmd.OutOrStdout()

	jsonOut, _ := cmd.Flags().GetBool("json")
	if jsonOut {
		// No sweep on the JSON path: the payload has no field to report the
		// count in, and a mutation a diagnostic cannot mention stays silent.
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
		return writeDoctorBundleIfAsked(cmd, cmd.ErrOrStderr(), res, contenoxDir, dbPath)
	}
	// A mission unit is a child subprocess of the host that fired it, so a
	// host that died left its row open with a heartbeat that will never
	// advance. Doctor is an operator's own read of runtime health, so it
	// reconciles the fleet it is about to report on.
	reclaimedMissions := reclaimAbandonedMissions(ctx, db, ResolveWorkspaceID(contenoxDir))

	printDoctorText(cmd.OutOrStdout(), res)
	printReclaimedMissions(cmd.OutOrStdout(), reclaimedMissions)
	printWorkspaceShadowNote(cmd.OutOrStdout(), contenoxDir, triggerShadowNames(o.EffectiveOptInBeta, contenoxDir))
	printVisionSummary(cmd.OutOrStdout(), vision)
	// One line, only when opted in; doctor says nothing about beta otherwise.
	if o.EffectiveOptInBeta {
		fmt.Fprintln(cmd.OutOrStdout(), "Beta features enabled (opt-in-beta): goja, shell_session, agent roster, event triggers")
		printLoadedTriggers(ctx, cmd.OutOrStdout(), contenoxDir)
		printFiringTrouble(ctx, cmd.OutOrStdout(), db.WithoutTransaction(), ResolveWorkspaceID(contenoxDir))
	}

	if err := writeDoctorBundleIfAsked(cmd, bundleW, res, contenoxDir, dbPath); err != nil {
		return err
	}

	// Advisory: warn when default-max-tokens exceeds the active provider's ceiling.
	store := runtimetypes.New(db.WithoutTransaction())
	maxTokStr := strings.TrimSpace(clikv.Read(ctx, store, "default-max-tokens"))
	if maxTokStr != "" {
		ceiling := res.DefaultMaxOutputTokens
		if ceiling > 0 {
			if n, convErr := strconv.Atoi(maxTokStr); convErr == nil && n > ceiling {
				fmt.Fprintf(cmd.OutOrStdout(),
					"\n⚠️  Advisory: default-max-tokens=%d exceeds %s provider ceiling (%d).\n"+
						"   Requests will be clamped automatically; set a lower value to silence this warning:\n"+
						"   contenox config set default-max-tokens %d\n",
					n, res.DefaultProvider, ceiling, ceiling)
			}
		}
	}
	return nil
}

// reclaimAbandonedMissions finishes missions whose host process is gone and
// returns how many (see missionservice.SweepAbandoned). Publisher-wired like
// every other reclaim seam: the terminal-status event must reach the bus (a
// live host reaps its zombie unit) and, under beta, the durable event log.
// Best-effort: doctor diagnoses an LLM setup, so a sweep failure is not one
// of the issues it reports.
func reclaimAbandonedMissions(ctx context.Context, db libdb.DBManager, workspaceID string) int {
	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	pub := missionEventPublisher(ctx, db, bus, workspaceID, libtracker.NoopTracker{}, nil)
	reclaimed, err := missionservice.New(db, missionservice.WithEventPublisher(pub)).SweepAbandoned(ctx)
	if err != nil {
		return 0
	}
	return reclaimed
}

// printReclaimedMissions names what the sweep just did, so the count is never
// a silent mutation behind a diagnostic. Prints nothing when it reclaimed
// nothing.
func printReclaimedMissions(w io.Writer, reclaimed int) {
	if reclaimed <= 0 {
		return
	}
	fmt.Fprintf(w, "Missions: %d reclaimed as %s — open with no heartbeat for %s, so their host process is gone (contenox mission list).\n",
		reclaimed, missionservice.StatusAbandoned, missionservice.StaleHeartbeatAfter)
}

// visionSummary is doctor's compact view of vision-capable model availability,
// derived from the synced runtime state (the same snapshot readiness used).
type visionSummary struct {
	// reachable is true when at least one backend synced without error; with
	// none, a vision line would be noise on top of the connectivity errors.
	reachable        bool
	visionModels     []string
	defaultHasVision bool
	defaultKnown     bool
}

// visionSummaryFromState collects vision-capable chat models and whether the
// configured default model is one of them.
func visionSummaryFromState(state map[string]runtimestate.BackendRuntimeState, defaultModel string) visionSummary {
	s := visionSummary{}
	seen := map[string]bool{}
	for _, bs := range state {
		if bs.Error != "" {
			continue
		}
		s.reachable = true
		for _, pm := range bs.PulledModels {
			if pm.Model == defaultModel || pm.Name == defaultModel {
				s.defaultKnown = true
				if pm.CanVision {
					s.defaultHasVision = true
				}
			}
			if !pm.CanVision || !pm.CanChat || seen[pm.Model] {
				continue
			}
			seen[pm.Model] = true
			s.visionModels = append(s.visionModels, pm.Model)
		}
	}
	sort.Strings(s.visionModels)
	return s
}

// printVisionSummary keeps doctor's vision line compact and teaching: how many
// vision-capable models exist, and whether an image request with the current
// default model would be refused.
func printVisionSummary(w io.Writer, v visionSummary) {
	if !v.reachable {
		return
	}
	if len(v.visionModels) == 0 {
		fmt.Fprintln(w, "Vision: no vision-capable models available — requests with images will be refused.")
		return
	}
	examples := v.visionModels
	if len(examples) > 3 {
		examples = examples[:3]
	}
	fmt.Fprintf(w, "Vision: %d model(s) accept images (e.g. %s).\n", len(v.visionModels), strings.Join(examples, ", "))
	if v.defaultKnown && !v.defaultHasVision {
		fmt.Fprintln(w, "        Note: the default model is text-only; image requests need a vision-capable model (name one or drop the pin).")
	}
}

// printWorkspaceShadowNote lists the system files (init's chain files and HITL
// policy presets, plus extra names such as the beta trigger-*.json set) whose
// workspace copy shadows a ~/.contenox copy, following the loaders'
// workspace-first resolution (lookupSystemFile, policyDirs). Informational,
// not an issue: shadowing is a feature (init --local). Prints nothing when no
// file is shadowed.
func printWorkspaceShadowNote(w io.Writer, contenoxDir string, extra []string) {
	home, err := globalContenoxDir()
	if err != nil || contenoxDir == "" || contenoxDir == home {
		return
	}
	var lines []string
	for _, name := range append(initSystemFileNames(), extra...) {
		wsPath := filepath.Join(contenoxDir, name)
		if _, err := os.Stat(wsPath); err != nil {
			continue
		}
		homePath := filepath.Join(home, name)
		if _, err := os.Stat(homePath); err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s (%s over %s)", name, wsPath, homePath))
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "\nWorkspace overrides (workspace copy wins):")
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

// doctorFallbackCommand is the next step for a not-ready runtime whose blocking
// issues carry no command of their own: the wizard registers a backend and sets
// both defaults, which is every remaining way to be not ready.
const doctorFallbackCommand = "contenox setup"

// doctorVerdict answers the question doctor is actually run to answer — can I
// chat right now — as a ready flag, the one blocking reason to name, and the
// single command that moves the operator closest to yes. Pure: it reads the
// already-computed Result and performs no I/O.
func doctorVerdict(res setupcheck.Result) (ready bool, reason, next string) {
	if res.Ready() {
		return true, "", ""
	}
	blocking := res.BlockingIssues()
	sort.SliceStable(blocking, func(i, j int) bool {
		ri, rj := issueRank(blocking[i].Code), issueRank(blocking[j].Code)
		if ri != rj {
			return ri < rj
		}
		return blocking[i].Code < blocking[j].Code
	})
	next = doctorFallbackCommand
	for _, iss := range blocking {
		if reason == "" {
			reason = iss.Message
		}
		if iss.CLICommand != "" {
			// The highest-ranked issue that names its own fix wins the "Next"
			// line even when a blocker above it had none.
			next = iss.CLICommand
			break
		}
	}
	return false, reason, next
}

// printDoctorVerdict leads the text report with that verdict, before any
// counts or per-backend detail. The --json payload is unchanged: its shape is
// a contract, and Ready() is already derivable from issues.
func printDoctorVerdict(w io.Writer, res setupcheck.Result) {
	ready, reason, next := doctorVerdict(res)
	if ready {
		fmt.Fprintln(w, "Ready: yes — chat now with `contenox new` or `contenox \"your prompt\"`.")
		fmt.Fprintln(w, "")
		return
	}
	if reason != "" {
		fmt.Fprintf(w, "Ready: no — %s\n", reason)
	} else {
		fmt.Fprintln(w, "Ready: no")
	}
	fmt.Fprintf(w, "Next:  %s\n", next)
	fmt.Fprintln(w, "")
}

func printDoctorText(w io.Writer, res setupcheck.Result) {
	printDoctorVerdict(w, res)
	fmt.Fprintf(w, "Default model:    %s\n", res.DefaultModel)
	fmt.Fprintf(w, "Default provider: %s\n", res.DefaultProvider)
	fmt.Fprintf(w, "Backends (registered): %d\n", res.BackendCount)
	fmt.Fprintf(w, "Reachable backends:    %d\n", res.ReachableBackendCount)
	// Fleet activity is process-lifetime; render nothing rather than "0/0/0".
	if c := fleetservice.Counters(); c.Dispatches > 0 || c.CapRefusals > 0 || c.VerificationDowngrades > 0 {
		fmt.Fprintf(w, "Fleet: %d dispatched, %d refused at cap, %d results downgraded\n",
			c.Dispatches, c.CapRefusals, c.VerificationDowngrades)
	}
	PrintBackendChecks(w, res)
	if len(res.Issues) == 0 {
		io.WriteString(w, "\n✓  All checks passed.\n")
		return
	}
	PrintSetupIssues(w, res)
}
