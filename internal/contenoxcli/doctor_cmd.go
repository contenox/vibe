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
	Short: "Print local LLM setup readiness (same evaluation as Beam GET /setup-status).",
	Long: `Runs the same backend sync and setup checks as the Beam onboarding API.

Examples:
  contenox doctor
  contenox doctor --json
  contenox doctor --skip-cycle   # faster; may show stale runtime state`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().Bool("json", false, "Print setupcheck.Result as JSON")
	doctorCmd.Flags().Bool("skip-cycle", false, "Skip RunBackendCycle (faster; state may be stale)")
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
	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(engine.SetupCheck)
	}
	printDoctorText(cmd.OutOrStdout(), engine.SetupCheck)
	return nil
}

func printDoctorText(w io.Writer, res setupcheck.Result) {
	fmt.Fprintf(w, "Default model:    %s\n", res.DefaultModel)
	fmt.Fprintf(w, "Default provider: %s\n", res.DefaultProvider)
	fmt.Fprintf(w, "Backends (registered): %d\n", res.BackendCount)
	fmt.Fprintf(w, "Reachable backends:    %d\n", res.ReachableBackendCount)
	PrintBackendChecks(w, res)
	if len(res.Issues) == 0 {
		io.WriteString(w, "\nNo issues reported.\n")
		return
	}
	PrintSetupIssues(w, res)
}
