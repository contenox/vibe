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
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/substrate"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/internal/version"
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
	jsonOut, _ := cmd.Flags().GetBool("json")

	stateStorage, err := substrate.Report(ctx, db.WithoutTransaction(), dbPath)
	if err != nil {
		return err
	}
	if unreachable := firstUnreachableSubstrate(stateStorage); unreachable != nil {
		return reportUnreachableSubstrate(cmd, jsonOut, stateStorage, unreachable, contenoxDir, dbPath)
	}

	// Built directly rather than via ComputeReadiness so the synced runtime state
	// is readable without a second backend cycle.
	engine, err := BuildEngine(ctx, db, o)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	res := setupcheck.EnrichResultWithOllamaProbe(ctx, engine.SetupCheck)
	res = setupcheck.AddStalePolicyPresetIssue(res, stalePolicyPresetIssues(operatorPolicyDirs(contenoxDir), betaGatedToolsets(o.EffectiveOptInBeta)), RefreshPoliciesCommand)
	res = setupcheck.AddTrustedBinaryIssue(res, trustedBinaryDrift(policyDirs(contenoxDir)), TrustBinariesRefreshCommand)
	vision := visionSummaryFromState(engine.State.Get(ctx), res.DefaultModel)
	engine.Stop()

	// stderr on the --json path so stdout stays a single parseable payload.
	bundleW := cmd.OutOrStdout()

	if jsonOut {
		// No sweep on the JSON path: the payload has no field to report it in.
		if err := encodeDoctorJSON(cmd.OutOrStdout(), res); err != nil {
			return err
		}
		return writeDoctorBundleIfAsked(cmd, cmd.ErrOrStderr(), res, contenoxDir, dbPath)
	}
	reclaimedMissions, reclaimErr := reclaimAbandonedMissions(ctx, db, ResolveWorkspaceID(contenoxDir))

	printDoctorText(cmd.OutOrStdout(), res)
	printStateStorage(cmd.OutOrStdout(), stateStorage)
	printReclaimedMissions(cmd.OutOrStdout(), reclaimedMissions)
	printMissionSweepFailure(cmd.OutOrStdout(), reclaimErr)
	printWorkspaceShadowNote(cmd.OutOrStdout(), contenoxDir, triggerShadowNames(o.EffectiveOptInBeta, contenoxDir))
	printVisionSummary(cmd.OutOrStdout(), vision)
	printBuildProvenance(cmd.OutOrStdout(), cliVersion(), version.GetProvenance())
	mcpServers, mcpErr := listAllMCPServers(ctx, runtimetypes.New(db.WithoutTransaction()))
	if mcpErr != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "MCP servers: unreadable (%v)\n", mcpErr)
	}
	printToolRoster(ctx, cmd.OutOrStdout(), acpRosterToolsets(o.EffectiveOptInBeta), mcpServers)
	if o.EffectiveOptInBeta {
		fmt.Fprintln(cmd.OutOrStdout(), "Beta features enabled (opt-in-beta): agent roster, event triggers")
		printLoadedTriggers(ctx, cmd.OutOrStdout(), contenoxDir)
		printFiringTrouble(ctx, cmd.OutOrStdout(), db.WithoutTransaction(), ResolveWorkspaceID(contenoxDir))
	}

	if err := writeDoctorBundleIfAsked(cmd, bundleW, res, contenoxDir, dbPath); err != nil {
		return err
	}

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

func reclaimAbandonedMissions(ctx context.Context, db libdb.DBManager, workspaceID string) (int, error) {
	bus, err := substrate.OpenBus(ctx, db.WithoutTransaction())
	if err != nil {
		return 0, err
	}
	defer bus.Close()
	pub := missionEventPublisher(ctx, db, bus, workspaceID, libtracker.NoopTracker{}, nil)
	reclaimed, err := missionservice.New(db, missionservice.WithEventPublisher(pub)).SweepAbandoned(ctx)
	if err != nil {
		return 0, err
	}
	return reclaimed, nil
}

func firstUnreachableSubstrate(statuses []substrate.Status) error {
	for _, s := range statuses {
		if s.Remote() && s.Err != nil {
			return fmt.Errorf("%s: cannot reach the %s server it names: %w", s.Setting, s.Backend, s.Err)
		}
	}
	return nil
}

func substrateUnreachableResult(err error) setupcheck.Result {
	return setupcheck.Result{Issues: []setupcheck.Issue{{
		Code:     "substrate_unreachable",
		Severity: "error",
		Category: "substrate",
		Message:  err.Error(),
	}}}
}

// reportUnreachableSubstrate emits what doctor already holds before returning the error that stopped it.
func reportUnreachableSubstrate(cmd *cobra.Command, jsonOut bool, statuses []substrate.Status, unreachable error, contenoxDir, dbPath string) error {
	res := substrateUnreachableResult(unreachable)
	bundleW := cmd.OutOrStdout()
	if jsonOut {
		if err := encodeDoctorJSON(cmd.OutOrStdout(), res); err != nil {
			return err
		}
		bundleW = cmd.ErrOrStderr()
	} else {
		printStateStorage(cmd.OutOrStdout(), statuses)
	}
	if err := writeDoctorBundleIfAsked(cmd, bundleW, res, contenoxDir, dbPath); err != nil {
		return err
	}
	return unreachable
}

func encodeDoctorJSON(w io.Writer, res setupcheck.Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

func printStateStorage(w io.Writer, statuses []substrate.Status) {
	if !substrate.AnyRemote(statuses) {
		return
	}
	io.WriteString(w, "\n")
	io.WriteString(w, "State storage:\n")
	for _, s := range statuses {
		if !s.Remote() {
			fmt.Fprintf(w, "  • %s: %s (%s)\n", s.Substrate, s.Backend, s.Target)
			io.WriteString(w, "    Status: local file\n")
			continue
		}
		fmt.Fprintf(w, "  • %s: %s (%s, from %s)\n", s.Substrate, s.Backend, s.Target, s.Setting)
		if s.Err == nil {
			io.WriteString(w, "    Status: reachable\n")
			continue
		}
		io.WriteString(w, "    Status: unreachable\n")
		fmt.Fprintf(w, "    Error: %s\n", s.Err)
		fmt.Fprintf(w, "    Hint: Start that server or unset %s; while it is set contenox never falls back to the local database.\n", s.Setting)
	}
}

func printMissionSweepFailure(w io.Writer, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(w, "Missions: the abandoned-mission sweep did not run (%v) — a mission whose host process is gone may still read as open.\n", err)
}

func printReclaimedMissions(w io.Writer, reclaimed int) {
	if reclaimed <= 0 {
		return
	}
	fmt.Fprintf(w, "Missions: %d reclaimed as %s — open with no heartbeat for at least %s, so their host process is gone (contenox mission list). A mission waiting on a pending ask is not reclaimed while that ask is still answerable: its bound widens to the ask's own window, and an ask with no deadline holds it open indefinitely (contenox mission show <id>).\n",
		reclaimed, missionservice.StatusAbandoned, missionservice.StaleHeartbeatAfter)
}

type visionSummary struct {
	reachable        bool
	visionModels     []string
	defaultHasVision bool
	defaultKnown     bool
}

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
			// Shipped chains live under system/; a workspace copy shadows those too.
			homePath = filepath.Join(systemDir(home), name)
			if _, err := os.Stat(homePath); err != nil {
				continue
			}
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

const doctorFallbackCommand = "contenox setup"

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
			// The highest-ranked issue that names its own fix wins the "Next" line.
			next = iss.CLICommand
			break
		}
	}
	return false, reason, next
}

func printDoctorVerdict(w io.Writer, res setupcheck.Result) {
	ready, reason, next := doctorVerdict(res)
	if ready {
		fmt.Fprintln(w, "Ready: yes — run: contenox beam")
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

// printBuildProvenance names the running build so a dirty working-tree build
// is distinguishable from the release its version.txt claims.
func printBuildProvenance(w io.Writer, v string, p version.Provenance) {
	if s := p.String(); s != "" {
		fmt.Fprintf(w, "\nBuild: %s (%s)\n", v, s)
		return
	}
	fmt.Fprintf(w, "\nBuild: %s\n", v)
}

// acpRosterToolsets is acpToolset composed with inert dependencies: the roster
// is enumerated here, never executed, so nothing may reach a DB, a fleet, or a
// live transport.
func acpRosterToolsets(optInBeta bool) map[string]taskengine.ToolsRepo {
	return acpToolset(acpProfileACP, nil, libtracker.NoopTracker{}, "",
		func(context.Context) *acpsvc.Transport { return nil },
		missionservice.New(nil), nil, nil, optInBeta,
		func() fleetservice.Service { return nil })
}

// listAllMCPServers pages through every registered MCP server, session-scoped
// ACP registrations included.
func listAllMCPServers(ctx context.Context, store runtimetypes.Store) ([]*runtimetypes.MCPServer, error) {
	var all []*runtimetypes.MCPServer
	var cursor *time.Time
	for {
		page, err := store.ListMCPServers(ctx, cursor, 100)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 100 {
			return all, nil
		}
		cursor = &page[len(page)-1].CreatedAt
	}
}

// printToolRoster names every tool `contenox acp` registers and what backs it.
// Doctor holds no ACP client, so a client-backed entry states the capability
// the editor must grant rather than a live verdict; an MCP server serves its
// own tools and is named per server.
func printToolRoster(ctx context.Context, w io.Writer, sets map[string]taskengine.ToolsRepo, servers []*runtimetypes.MCPServer) {
	fmt.Fprintln(w, "\nTool roster (tool — toolset — origin; what `contenox acp` registers):")
	names := make([]string, 0, len(sets))
	for name := range sets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		repo := sets[name]
		tools, err := acpsvc.PotentialClientTools(ctx, repo, name)
		if err != nil {
			fmt.Fprintf(w, "  %s — unavailable: %v\n", name, err)
			continue
		}
		if len(tools) == 0 {
			// missiontools advertises by session role (mission unit vs supervisor).
			fmt.Fprintf(w, "  %s — local (in-process); tools advertised per session role\n", name)
			continue
		}
		clientBacked := acpsvc.IsClientBacked(repo)
		for _, tool := range tools {
			origin := "local (in-process)"
			if clientBacked {
				origin = "needs client capability " + acpsvc.RequiredClientCapability(name, tool.Function.Name)
			}
			fmt.Fprintf(w, "  %s — %s — %s\n", tool.Function.Name, name, origin)
		}
	}
	for _, srv := range servers {
		if runtimetypes.IsACPManagedMCPServerName(srv.Name) {
			fmt.Fprintf(w, "  %s — MCP server (session-scoped, supplied by an attached client)\n", srv.Name)
			continue
		}
		fmt.Fprintf(w, "  %s — MCP server (%s); its tools are served live per session\n", srv.Name, mcpServerTarget(srv))
	}
	fmt.Fprintf(w, "  %s — not mounted under `contenox serve`: %s\n",
		strings.Join(hostUnservedToolsets, ", "), hostUnservedToolsetRefusal)
}

// mcpServerTarget is the transport plus whichever endpoint the server has: a
// URL for sse/http, a command for stdio.
func mcpServerTarget(srv *runtimetypes.MCPServer) string {
	switch {
	case srv.URL != "":
		return srv.Transport + " " + srv.URL
	case srv.Command != "":
		return srv.Transport + " " + srv.Command
	default:
		return srv.Transport
	}
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
