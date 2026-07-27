package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/fleetservice"
	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check LLM setup: defaults, registered backends, and connectivity.",
	Long: `Shows whether your default model and provider are set, lists every registered backend
(Ollama, OpenAI, Gemini, vLLM, Vertex AI), and reports reachability and setup
issues for each. Use it after contenox init, after contenox backend add, or when
chat/run cannot resolve a model.

Additionally, if you use local Ollama: when no Ollama backend is ready yet, doctor may probe
your Ollama URL (OLLAMA_HOST, or http://127.0.0.1:11434) and suggest commands to pull a model
(at least ollama pull qwen2.5:7b), register the backend, and set defaults—including --url for a
non-default host or port.

Examples:
  contenox doctor
  contenox doctor --json
  contenox doctor --skip-cycle`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().Bool("json", false, "Print results as JSON")
	doctorCmd.Flags().Bool("skip-cycle", false, "Skip syncing backends (faster; status may be outdated)")
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

	res, err := ComputeReadiness(ctx, db, o)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}

	jsonOut, _ := cmd.Flags().GetBool("json")
	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	printDoctorText(cmd.OutOrStdout(), res)

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

func printDoctorText(w io.Writer, res setupcheck.Result) {
	fmt.Fprintf(w, "Default model:    %s\n", res.DefaultModel)
	fmt.Fprintf(w, "Default provider: %s\n", res.DefaultProvider)
	fmt.Fprintf(w, "Backends (registered): %d\n", res.BackendCount)
	fmt.Fprintf(w, "Reachable backends:    %d\n", res.ReachableBackendCount)
	// Fleet activity is process-lifetime, so this line only means something in
	// a long-lived process (an editor session); a fresh doctor run shows it only
	// if this very invocation dispatched — render nothing rather than "0/0/0".
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
