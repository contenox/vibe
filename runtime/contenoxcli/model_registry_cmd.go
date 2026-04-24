package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"text/tabwriter"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/modelregistry"
	"github.com/contenox/contenox/runtime/modelregistryservice"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var modelAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Register a model in the local registry without downloading.",
	Long: `Register a model name and its source URL in the local model registry.
This does not download the model; use 'model pull' to download.

Examples:
  contenox model add my-llm --url https://huggingface.co/org/model.gguf
  contenox model add my-llm --url https://huggingface.co/org/model.gguf --size 1073741824`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]
		rawURL, _ := cmd.Flags().GetString("url")
		size, _ := cmd.Flags().GetInt64("size")

		db, svc, _, err := openModelRegistryDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		e := &runtimetypes.ModelRegistryEntry{
			ID:        uuid.NewString(),
			Name:      name,
			SourceURL: rawURL,
			SizeBytes: size,
		}
		if err := svc.Create(ctx, e); err != nil {
			return fmt.Errorf("failed to add model registry entry: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Model %q registered in local registry.\n", name)
		return nil
	},
}

var modelShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show details for a model from the registry.",
	Long: `Resolve a model by name from the registry (curated + user-added) and print its details.

Examples:
  contenox model show qwen2.5-1.5b
  contenox model show my-custom-llm`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, _, reg, err := openModelRegistryDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		d, err := reg.Resolve(ctx, args[0])
		if err != nil {
			return fmt.Errorf("model %q not found in registry: %w", args[0], err)
		}
		data, _ := json.MarshalIndent(d, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	},
}

var modelRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a user-registered model from the local registry.",
	Long: `Delete a user-added model registry entry by name.
Curated (built-in) models cannot be removed.

Examples:
  contenox model remove my-custom-llm
  contenox model rm my-custom-llm`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, svc, _, err := openModelRegistryDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		e, err := svc.GetByName(ctx, args[0])
		if err != nil {
			return fmt.Errorf("model %q not found in user registry: %w", args[0], err)
		}
		if err := svc.Delete(ctx, e.ID); err != nil {
			return fmt.Errorf("failed to remove model registry entry: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Model %q removed from local registry.\n", args[0])
		return nil
	},
}

var modelRegistryListCmd = &cobra.Command{
	Use:   "registry-list",
	Short: "List all models in the registry (curated + user-added).",
	Long: `Show all models known to the registry: built-in curated entries and any user-added entries.

Examples:
  contenox model registry-list`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, _, reg, err := openModelRegistryDB(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		entries, err := reg.List(ctx)
		if err != nil {
			return fmt.Errorf("failed to list registry: %w", err)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSIZE (MB)\tCURATED\tSOURCE")
		for _, e := range entries {
			curated := "-"
			if e.Curated {
				curated = "✓"
			}
			sizeMB := e.SizeBytes / 1024 / 1024
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", e.Name, sizeMB, curated, e.SourceURL)
		}
		return w.Flush()
	},
}

func openModelRegistryDB(cmd *cobra.Command) (libdb.DBManager, modelregistryservice.Service, modelregistry.Registry, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return nil, nil, nil, err
	}
	svc := modelregistryservice.New(db)
	reg := modelregistry.New(svc)
	return db, svc, reg, nil
}

func init() {
	modelAddCmd.Flags().String("url", "", "Source URL for the model GGUF file (required)")
	modelAddCmd.Flags().Int64("size", 0, "Approximate model size in bytes (optional)")
	_ = modelAddCmd.MarkFlagRequired("url")

	modelCmd.AddCommand(modelAddCmd)
	modelCmd.AddCommand(modelShowCmd)
	modelCmd.AddCommand(modelRemoveCmd)
	modelCmd.AddCommand(modelRegistryListCmd)
}
