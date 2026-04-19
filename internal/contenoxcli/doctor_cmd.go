package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/contenox/contenox/internal/setupcheck"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check LLM setup: defaults, registered backends, and connectivity.",
	Long: `Shows whether your default model and provider are set, lists every registered backend
(OpenAI, Gemini, Ollama, vLLM, etc.), and reports reachability and setup issues for each.
Use it after contenox init, after contenox backend add, or when chat/plan cannot resolve a model.

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

	o := buildRunOpts(cmd, db, contenoxDir)
	o.EffectiveDB = dbPath
	o.EffectiveSkipBackendCycle, _ = cmd.Flags().GetBool("skip-cycle")

	engine, err := BuildEngine(ctx, db, o)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	jsonOut, _ := cmd.Flags().GetBool("json")
	res := setupcheck.EnrichResultWithOllamaProbe(ctx, engine.SetupCheck)
	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	printDoctorText(cmd.OutOrStdout(), res)
	return nil
}

func printDoctorText(w io.Writer, res setupcheck.Result) {
	fmt.Fprintf(w, "Default model:    %s\n", res.DefaultModel)
	fmt.Fprintf(w, "Default provider: %s\n", res.DefaultProvider)
	fmt.Fprintf(w, "Backends (registered): %d\n", res.BackendCount)
	fmt.Fprintf(w, "Reachable backends:    %d\n", res.ReachableBackendCount)
	PrintBackendChecks(w, res)
	if len(res.Issues) == 0 {
		io.WriteString(w, "\n✓  All checks passed. Run 'contenox beam' to start.\n")
		return
	}
	PrintSetupIssues(w, res)
}
