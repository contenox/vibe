package contenoxcli

import (
	"context"
	"fmt"
	"strings"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/modelcapability"
	"github.com/spf13/cobra"
)

var modelCapabilityCmd = &cobra.Command{
	Use:   "capability",
	Short: "Manage manual provider/model capability overrides.",
	Long: `Manage manual capability overrides for a specific provider and model.

Use this when a provider catalog does not advertise a capability that you know
is supported, or when you need to suppress a capability for a specific endpoint.
Overrides are scoped by provider and model name and are applied before runtime
providers are constructed.

Examples:
  contenox model capability set openai gpt-5-mini --think true
  contenox model capability set vllm Qwen/Qwen3-32B --think false
  contenox model capability set ollama my-vlm --vision true
  contenox model capability show openai gpt-5-mini
  contenox model capability unset openai gpt-5-mini`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var modelCapabilitySetCmd = &cobra.Command{
	Use:   "set <provider> <model>",
	Short: "Set a manual capability override.",
	Long: `Set a manual provider/model capability override.

The --think flag records whether this provider/model supports reasoning request
controls. This is different from --think on chat/run, which selects a reasoning
level for one invocation.

The --vision flag records whether this provider/model accepts image input. The
resolver routes image-bearing requests only to vision-capable models.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, svc, err := openModelCapabilityService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		thinkRaw, _ := cmd.Flags().GetString("think")
		visionRaw, _ := cmd.Flags().GetString("vision")
		if strings.TrimSpace(thinkRaw) == "" && strings.TrimSpace(visionRaw) == "" {
			return fmt.Errorf("provide at least one capability flag: --think or --vision")
		}
		var reported []string
		if strings.TrimSpace(thinkRaw) != "" {
			canThink, err := parseModelCapabilityBool("--think", thinkRaw)
			if err != nil {
				return err
			}
			if _, err := svc.SetThink(ctx, args[0], args[1], canThink); err != nil {
				return fmt.Errorf("failed to set capability override: %w", err)
			}
			reported = append(reported, fmt.Sprintf("think=%t", canThink))
		}
		var override *modelcapability.Override
		if strings.TrimSpace(visionRaw) != "" {
			canVision, err := parseModelCapabilityBool("--vision", visionRaw)
			if err != nil {
				return err
			}
			if override, err = svc.SetVision(ctx, args[0], args[1], canVision); err != nil {
				return fmt.Errorf("failed to set capability override: %w", err)
			}
			reported = append(reported, fmt.Sprintf("vision=%t", canVision))
		}
		_, provider, model, keyErr := modelcapability.Key(args[0], args[1])
		if keyErr != nil {
			return keyErr
		}
		if override != nil {
			provider, model = override.Provider, override.Model
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Capability override set for %s/%s: %s.\n", provider, model, strings.Join(reported, ", "))
		return nil
	},
}

var modelCapabilityShowCmd = &cobra.Command{
	Use:   "show <provider> <model>",
	Short: "Show a manual capability override.",
	Long: `Print the manual capability override recorded for a provider/model pair.
Reports the think (reasoning controls) and vision (image input) settings. If no
override is recorded for the pair, prints that none exists.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, svc, err := openModelCapabilityService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		override, ok, err := svc.Get(ctx, args[0], args[1])
		if err != nil {
			return fmt.Errorf("failed to read capability override: %w", err)
		}
		if !ok || (override.CanThink == nil && override.CanVision == nil) {
			_, provider, model, keyErr := modelcapability.Key(args[0], args[1])
			if keyErr != nil {
				return keyErr
			}
			fmt.Fprintf(cmd.OutOrStdout(), "No capability override for %s/%s.\n", provider, model)
			return nil
		}
		var parts []string
		if override.CanThink != nil {
			parts = append(parts, fmt.Sprintf("think=%t", *override.CanThink))
		}
		if override.CanVision != nil {
			parts = append(parts, fmt.Sprintf("vision=%t", *override.CanVision))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Capability override for %s/%s: %s.\n", override.Provider, override.Model, strings.Join(parts, ", "))
		return nil
	},
}

var modelCapabilityUnsetCmd = &cobra.Command{
	Use:   "unset <provider> <model>",
	Short: "Remove a manual capability override.",
	Long: `Remove the manual capability override for a provider/model pair, reverting to
whatever the provider catalog advertises. Reports whether an override was
actually present to remove.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, svc, err := openModelCapabilityService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		removed, err := svc.Unset(ctx, args[0], args[1])
		if err != nil {
			return fmt.Errorf("failed to remove capability override: %w", err)
		}
		_, provider, model, keyErr := modelcapability.Key(args[0], args[1])
		if keyErr != nil {
			return keyErr
		}
		if !removed {
			fmt.Fprintf(cmd.OutOrStdout(), "No capability override for %s/%s.\n", provider, model)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Capability override removed for %s/%s.\n", provider, model)
		return nil
	},
}

func parseModelCapabilityBool(flag, value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", flag)
	}
}

func openModelCapabilityService(cmd *cobra.Command) (libdb.DBManager, modelcapability.Service, error) {
	db, store, err := openConfigDB(cmd)
	if err != nil {
		return nil, modelcapability.Service{}, err
	}
	return db, modelcapability.New(store), nil
}

func init() {
	modelCapabilitySetCmd.Flags().String("think", "", "Whether this provider/model supports thinking/reasoning controls (true or false).")
	modelCapabilitySetCmd.Flags().String("vision", "", "Whether this provider/model accepts image input (true or false).")

	modelCapabilityCmd.AddCommand(modelCapabilitySetCmd)
	modelCapabilityCmd.AddCommand(modelCapabilityShowCmd)
	modelCapabilityCmd.AddCommand(modelCapabilityUnsetCmd)
	modelCmd.AddCommand(modelCapabilityCmd)
}
