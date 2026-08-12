package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Inspect and manage the runtime's declared agents.",
	Long: `List, show, and manage the agents the runtime can spawn and drive.

Agents are the runtime's own task chains, discovered from chain files on disk
and addressable as ACP peers. They are registered automatically by chain-agent
discovery; this command inspects them and toggles their enabled state.

Examples:
  contenox agent list
  contenox agent show agent-reviewer
  contenox agent disable agent-reviewer
  contenox agent remove agent-reviewer`,
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
			fmt.Fprintln(cmd.OutOrStdout(), "No agents registered.")
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
			return fmt.Errorf("agent %q not found: %w", name, err)
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
			return fmt.Errorf("agent %q not found: %w", name, err)
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
		return fmt.Errorf("agent %q not found: %w", name, err)
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
	return db, agentregistryservice.New(db), nil
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
