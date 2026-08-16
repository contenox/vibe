package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Inspect and manage the runtime's declared agents.",
	Long: `List, show, and manage the agents the runtime can spawn and drive.

An agent is a Markdown file with a YAML frontmatter header:

  ---
  name: reviewer
  description: Reviews a file for correctness problems
  tools: Read, Glob, Grep
  ---

  You are a code reviewer. Read the file you are asked about, then list the
  problems you can point at in what you actually read.

Write them in .contenox/agents/ (or ~/.contenox/agents/ for every project).
Agents you already keep in .claude/agents/ or .agents/agents/ are read where
they are — nothing to move or convert. Every one is registered automatically;
this command inspects them and toggles their enabled state.

What a declaration cannot say — context budget, retries, shell allowlists, what
needs a human — lives in agents.toml beside them.

Examples:
  contenox agent list
  contenox agent show reviewer
  contenox agent disable reviewer
  contenox agent remove reviewer`,
}

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered agents.",
	Long: `List every registered agent as a table of id, name, source, kind, and enabled
state. If none are registered, prints a hint.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, svc, err := openAgentService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		agents, err := svc.List(ctx, nil, 100)
		if err != nil {
			return fmt.Errorf("failed to list agents: %w", err)
		}
		if len(agents) == 0 {
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "No agents yet.")
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "An agent is one Markdown file. Write .contenox/agents/reviewer.md:")
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "  ---")
			fmt.Fprintln(out, "  name: reviewer")
			fmt.Fprintln(out, "  description: Reviews a file for correctness problems")
			fmt.Fprintln(out, "  tools: Read, Glob, Grep")
			fmt.Fprintln(out, "  ---")
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "  You are a code reviewer. Read the file you are asked about, then")
			fmt.Fprintln(out, "  list the problems you can point at in what you actually read.")
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "Then run this again. Agents already in .claude/agents/ are found too.")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSOURCE\tKIND\tENABLED")
		for _, a := range agents {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\n", a.ID, a.Name, derefOr(a.Source, "-"), a.Kind, a.Enabled)
		}
		return w.Flush()
	},
}

var agentShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show an agent's full declaration and run config.",
	Long: `Look up an agent by name and print its provenance and raw config_json.
Provenance (source, registry id/version) is system-managed.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]
		db, svc, err := openAgentService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		agent, err := svc.GetByName(ctx, name)
		if err != nil {
			return fmt.Errorf("agent %q not found%s: %w", name, legacyChainPrefixHint(name), err)
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Name:     %s\n", agent.Name)
		fmt.Fprintf(out, "ID:       %s\n", agent.ID)
		fmt.Fprintf(out, "Kind:     %s\n", agent.Kind)
		fmt.Fprintf(out, "Enabled:  %v\n", agent.Enabled)
		fmt.Fprintf(out, "Source:   %s\n", derefOr(agent.Source, "-"))
		if agent.RegistryID != nil {
			fmt.Fprintf(out, "Registry: %s@%s\n", *agent.RegistryID, derefOr(agent.RegistryVersion, "?"))
		}

		pretty, err := prettyJSON(agent.ConfigJSON)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "\nConfig (config_json):")
		fmt.Fprintln(out, pretty)
		return nil
	},
}

var agentRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a registered agent.",
	Long: `Delete an agent by name from the local database. This removes only the local
registration; discovery may re-register it on the next startup if its chain
file still exists.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]
		db, svc, err := openAgentService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		agent, err := svc.GetByName(ctx, name)
		if err != nil {
			return fmt.Errorf("agent %q not found%s: %w", name, legacyChainPrefixHint(name), err)
		}
		if err := svc.Delete(ctx, agent.ID); err != nil {
			return fmt.Errorf("failed to remove agent: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Agent %q removed.\n", name)
		return nil
	},
}

var agentEnableCmd = &cobra.Command{
	Use:   "enable <name>",
	Short: "Enable a registered agent.",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return setAgentEnabled(cmd, args[0], true) },
}

var agentDisableCmd = &cobra.Command{
	Use:   "disable <name>",
	Short: "Disable a registered agent.",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return setAgentEnabled(cmd, args[0], false) },
}

func setAgentEnabled(cmd *cobra.Command, name string, enabled bool) error {
	ctx := libtracker.WithNewRequestID(context.Background())
	db, svc, err := openAgentService(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	agent, err := svc.GetByName(ctx, name)
	if err != nil {
		return fmt.Errorf("agent %q not found%s: %w", name, legacyChainPrefixHint(name), err)
	}
	if agent.Enabled == enabled {
		fmt.Fprintf(cmd.OutOrStdout(), "Agent %q already %s.\n", name, enabledWord(enabled))
		return nil
	}
	agent.Enabled = enabled
	if err := svc.Update(ctx, agent); err != nil {
		return fmt.Errorf("failed to update agent: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Agent %q %s.\n", name, enabledWord(enabled))
	return nil
}

// legacyChainPrefixHint explains a name that would have resolved before declared
// agents dropped the chain- prefix from their id; empty for anything else.
func legacyChainPrefixHint(name string) string {
	bare := strings.TrimPrefix(name, "chain-")
	if bare == name || bare == "" {
		return ""
	}
	return fmt.Sprintf(" — declared agents no longer carry the chain- prefix; try %q", bare)
}

func openAgentService(cmd *cobra.Command) (libdb.DBManager, agentregistryservice.Service, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid database path: %w", err)
	}
	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	agents := agentregistryservice.New(db)
	// Declarations are the source of truth, so inspecting the roster runs a
	// discovery pass first, and prints whatever that pass could not act on.
	if contenoxDir, dirErr := ResolveContenoxDir(cmd); dirErr == nil {
		printSyncProblems(cmd.ErrOrStderr(), discoverChainAgentsReporting(dbCtx, agents, contenoxDir, libtracker.NoopTracker{}, DiscoverDeps{Store: runtimetypes.New(db.WithoutTransaction())}))
	}
	return db, agents, nil
}

func prettyJSON(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "{}", nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return "", fmt.Errorf("format config JSON: %w", err)
	}
	return buf.String(), nil
}

func derefOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

func enabledWord(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func init() {
	agentCmd.AddCommand(agentListCmd)
	agentCmd.AddCommand(agentShowCmd)
	agentCmd.AddCommand(agentRemoveCmd)
	agentCmd.AddCommand(agentEnableCmd)
	agentCmd.AddCommand(agentDisableCmd)
}
