package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/contenox/contenox/runtime/internal/runtimestate"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/modelservice"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/spf13/cobra"
)

var modelCmd = &cobra.Command{
	Use:     "model",
	Aliases: []string{"models"},
	Short:   "Inspect LLM models served by backends.",
	Long: `Inspect models available from LLM backends.

By default, 'model list' queries each registered backend in real-time and
shows the models it is currently serving.

Examples:
  contenox-runtime model list

Set the default model:
  contenox-runtime config set default-model    gemini-2.5-flash
  contenox-runtime config set default-provider gemini`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return fmt.Errorf("unknown subcommand %q\n\nTo set a default model:\n  contenox-runtime config set default-model <model>\n  contenox-runtime config set default-provider <provider>", args[0])
		}
		return cmd.Help()
	},
}

var modelListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List models available from live backends.",
	Long: `Query each registered backend in real time and show its available models.

Shows model name, backend it comes from, and capabilities discovered at runtime
(chat, embed, prompt, stream, context length).

Examples:
  contenox-runtime model list`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, _, err := openBackendDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()
		return printLiveModels(ctx, db, cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

// printLiveModels runs one backend reconciliation cycle and prints what each
// backend is actually serving right now.
func printLiveModels(ctx context.Context, db libdb.DBManager, out, errW io.Writer) error {
	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()

	// Read the preferred model from config so we can mark it.
	store := runtimetypes.New(db.WithoutTransaction())
	preferredModel, err := getConfigKV(ctx, store, "default-model")
	if err != nil {
		return fmt.Errorf("failed to get preferred model: %w", err)
	}

	state, err := runtimestate.New(ctx, db, bus, runtimestate.WithSkipDeleteUndeclaredModels(), runtimestate.WithAutoDiscoverModels())
	if err != nil {
		return fmt.Errorf("failed to initialize runtime state: %w", err)
	}

	// A single cycle contacts every backend and populates PulledModels.
	if err := state.RunBackendCycle(ctx); err != nil {
		// Non-fatal: partial results are still useful.
		fmt.Fprintf(errW, "warning: backend cycle error: %v\n", err)
	}

	rt := state.Get(ctx)
	if len(rt) == 0 {
		fmt.Fprintln(out, "No backends registered. Run: contenox-runtime backend add <name> --type <type>")
		return nil
	}

	// Stable sort by backend name.
	type entry struct {
		backendName string
		backendErr  string
		pulled      []string
		canChat     map[string]bool
		canEmbed    map[string]bool
		canPrompt   map[string]bool
		ctx         map[string]int
	}
	var entries []entry
	for _, bs := range rt {
		e := entry{
			backendName: bs.Name,
			backendErr:  bs.Error,
			canChat:     map[string]bool{},
			canEmbed:    map[string]bool{},
			canPrompt:   map[string]bool{},
			ctx:         map[string]int{},
		}
		for _, pm := range bs.PulledModels {
			e.pulled = append(e.pulled, pm.Model)
			e.canChat[pm.Model] = pm.CanChat
			e.canEmbed[pm.Model] = pm.CanEmbed
			e.canPrompt[pm.Model] = pm.CanPrompt
			e.ctx[pm.Model] = pm.ContextLength
		}
		// Some providers only report model names; when the backend is healthy,
		// keep those visible even if no detailed PulledModels entries were built.
		if len(e.pulled) == 0 && bs.Error == "" && len(bs.Models) > 0 {
			e.pulled = append(e.pulled, bs.Models...)
		}
		sort.Strings(e.pulled)
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].backendName < entries[j].backendName })

	any := false
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BACKEND\tMODEL\tCHAT\tEMBED\tPROMPT\tCTX")
	for _, e := range entries {
		if e.backendErr != "" {
			errMsg := e.backendErr
			if len(errMsg) > 80 {
				errMsg = errMsg[:80] + "..."
			}
			fmt.Fprintf(w, "%s\t(unreachable: %s)\t\t\t\t\n", e.backendName, errMsg)
			continue
		}
		if len(e.pulled) == 0 {
			fmt.Fprintf(w, "%s\t(no models)\t\t\t\t\n", e.backendName)
			continue
		}
		for _, m := range e.pulled {
			any = true
			displayName := m
			if preferredModel != "" && m == preferredModel {
				displayName = m + " *"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n",
				e.backendName, displayName,
				boolMark(e.canChat[m]),
				boolMark(e.canEmbed[m]),
				boolMark(e.canPrompt[m]),
				e.ctx[m],
			)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if !any {
		fmt.Fprintln(out, "\nNo models found on any backend.")
	}
	if preferredModel != "" {
		fmt.Fprintln(out, "\n* = default model (contenox-runtime config set default-model <name>)")
	}
	return nil
}

func boolMark(b bool) string {
	if b {
		return "✓"
	}
	return "-"
}

// parseContextSize converts a human-friendly token-count string to an int.
// Accepted suffixes (case-insensitive): k (×1 000), m (×1 000 000).
// A bare integer is returned as-is.  Examples:
//
//	"12k" → 12000
//	"128K" → 128000
//	"1m"  → 1000000
//	"8192" → 8192
//	"0"   → 0  (API-authoritative)
func parseContextSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("context size must not be empty")
	}
	last := rune(s[len(s)-1])
	var multiplier int64 = 1
	numPart := s
	if unicode.IsLetter(last) {
		numPart = s[:len(s)-1]
		switch unicode.ToLower(last) {
		case 'k':
			multiplier = 1_000
		case 'm':
			multiplier = 1_000_000
		default:
			return 0, fmt.Errorf("unknown suffix %q: use k (thousands) or m (millions)", string(last))
		}
	}
	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid context size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("context size must be ≥ 0, got %d", n)
	}
	return int(n * multiplier), nil
}

var modelSetContextCmd = &cobra.Command{
	Use:   "set-context <model-name>",
	Short: "Set a local context override for a model.",
	Long: `Override the locally stored context window for a model already known to the local runtime state.

Accepts a bare integer or a k/m shorthand (case-insensitive):
  k  – thousands   (12k  = 12 000)
  m  – millions    (1m   = 1 000 000)

Examples:
  contenox-runtime model set-context gpt-5-mini           --context 128k
  contenox-runtime model set-context gemini-3.1-pro-preview --context 1m
  contenox-runtime model set-context qwen2.5:7b             --context 32k`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, _, err := openBackendDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		ctxRaw, _ := cmd.Flags().GetString("context")
		ctxLen, err := parseContextSize(ctxRaw)
		if err != nil {
			return fmt.Errorf("--context: %w", err)
		}
		modelName := args[0]
		store := runtimetypes.New(db.WithoutTransaction())
		m, err := store.GetModelByName(ctx, modelName)
		if err != nil {
			return fmt.Errorf("model %q has no local override row yet: %w", modelName, err)
		}
		m.ContextLength = ctxLen
		if err := modelservice.New(db, "").Update(ctx, m); err != nil {
			return fmt.Errorf("failed to update model: %w", err)
		}
		if ctxLen == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Model %q context cleared (API is authoritative).\n", modelName)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Model %q context set to %d.\n", modelName, ctxLen)
		}
		return nil
	},
}

func init() {
	modelSetContextCmd.Flags().String("context", "", "Context window size: bare int or shorthand (12k, 128k, 1m).")
	_ = modelSetContextCmd.MarkFlagRequired("context")
	modelCmd.AddCommand(modelListCmd)
	modelCmd.AddCommand(modelSetContextCmd)
}
