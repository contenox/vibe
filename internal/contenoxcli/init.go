// init.go implements the contenox init subcommand (scaffold .contenox/).
package contenoxcli

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtimetypes"
)

//go:embed chain-contenox.json
var initChain string

//go:embed chain-run.json
var initRunChain string

// providerConfig holds the provider-specific values used during init.
type providerConfig struct {
	name         string
	defaultModel string
	envKey       string
}

var providerConfigs = map[string]providerConfig{
	"ollama": {
		name:         "Ollama (local)",
		defaultModel: defaultModel,
		envKey:       "",
	},
	"gemini": {
		name:         "Google Gemini",
		defaultModel: "gemini-3.1-pro-preview",
		envKey:       "GEMINI_API_KEY",
	},
	"openai": {
		name:         "OpenAI",
		defaultModel: "gpt-5-mini",
		envKey:       "OPENAI_API_KEY",
	},
}

// RunInit scaffolds .contenox/ with default chain files.
// provider is "" (default = ollama), "ollama", "gemini", or "openai".
// contenoxDir is the target data directory (e.g. from --data-dir or the default .contenox/).
func RunInit(out, errOut io.Writer, force bool, provider string, contenoxDir string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = "ollama"
	}

	pc, ok := providerConfigs[provider]
	if !ok {
		return fmt.Errorf("unknown provider %q — valid options: ollama, gemini, openai", provider)
	}
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return fmt.Errorf("failed to create .contenox directory: %w", err)
	}
	chainPath := filepath.Join(contenoxDir, "default-chain.json")
	runChainPath := filepath.Join(contenoxDir, "default-run-chain.json")
	writeFile := func(path, content string) error {
		if !force {
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(out, "  %s already exists (use --force to overwrite)\n", path)
				return nil
			}
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		fmt.Fprintf(out, "  Created %s\n", path)
		return nil
	}

	if err := writeFile(chainPath, initChain); err != nil {
		return err
	}
	if err := writeFile(runChainPath, initRunChain); err != nil {
		return err
	}

	plannerPath, executorPath, wrotePlanner, wroteExecutor, err := writeEmbeddedPlanChains(contenoxDir, force)
	if err != nil {
		return err
	}
	if !wrotePlanner {
		fmt.Fprintf(out, "  %s already exists (use --force to overwrite)\n", plannerPath)
	} else {
		fmt.Fprintf(out, "  Created %s\n", plannerPath)
	}
	if !wroteExecutor {
		fmt.Fprintf(out, "  %s already exists (use --force to overwrite)\n", executorPath)
	} else {
		fmt.Fprintf(out, "  Created %s\n", executorPath)
	}
	fmt.Fprintln(out, "  Plan commands use these chains; running 'contenox plan new' or 'plan next' refreshes them from the binary.")
	fmt.Fprintln(out, "  After registering a backend, run 'contenox doctor' to verify setup before planning.")

	fmt.Fprintln(out, "Done.")
	fmt.Fprintln(out, "")

	// Surface the currently configured model so users immediately know
	// if they have a stale entry from a previous install.
	dbPath := filepath.Join(contenoxDir, "local.db")
	if _, statErr := os.Stat(dbPath); statErr == nil {
		if db, openErr := OpenDBAt(libtracker.WithNewRequestID(context.Background()), dbPath); openErr == nil {
			store := runtimetypes.New(db.WithoutTransaction())
			ctx := libtracker.WithNewRequestID(context.Background())
			curModel, err := getConfigKV(ctx, store, "default-model")
			if err != nil {
				return err
			}
			curProvider, err := getConfigKV(ctx, store, "default-provider")
			if err != nil {
				return err
			}
			db.Close()
			if curModel != "" || curProvider != "" {
				fmt.Fprintln(out, "Current config (from local.db):")
				if curProvider != "" {
					fmt.Fprintf(out, "  default-provider = %s\n", curProvider)
				}
				if curModel != "" {
					fmt.Fprintf(out, "  default-model    = %s\n", curModel)
				}
				fmt.Fprintln(out, "  To change: contenox config set default-model <model>")
				fmt.Fprintln(out, "")
			}
		}
	}

	// If a cloud provider is selected, check for the API key and instruct if missing.
	if pc.envKey != "" {
		if os.Getenv(pc.envKey) == "" {
			fmt.Fprintf(out, "⚠️  %s API key not found in environment.\n", pc.name)
			fmt.Fprintf(out, "   Set it before running contenox:\n\n")
			fmt.Fprintf(out, "     export %s=your-key-here\n\n", pc.envKey)
		} else {
			fmt.Fprintf(out, "✓  %s API key detected (%s).\n\n", pc.name, pc.envKey)
		}
	}

	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintln(out, "")
	if provider == "ollama" {
		fmt.Fprintln(out, "  1. Install Ollama (if not already):")
		fmt.Fprintln(out, "       curl -fsSL https://ollama.com/install.sh | sh")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  2. Start Ollama and pull a model the runtime can observe:")
		fmt.Fprintln(out, "       ollama serve && ollama pull qwen2.5:7b")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  Optional: use hosted Ollama Cloud instead of a local server:")
		fmt.Fprintln(out, "       export OLLAMA_API_KEY=your-key-here")
		fmt.Fprintln(out, "       contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY")
		fmt.Fprintln(out, "")
	} else {
		fmt.Fprintf(out, "  1. Register the %s backend:\n", pc.name)
		fmt.Fprintf(out, "       contenox backend add %s --type %s --api-key-env %s\n", provider, provider, pc.envKey)
		fmt.Fprintf(out, "       contenox model list   # confirm the runtime can see %s models\n", pc.name)
		fmt.Fprintf(out, "       contenox config set default-model %s\n", pc.defaultModel)
		fmt.Fprintln(out, "")
	}
	// Print API key link for cloud providers
	switch provider {
	case "ollama":
		fmt.Fprintln(out, "  Get an Ollama API key for direct cloud access: https://ollama.com/settings/keys")
		fmt.Fprintln(out, "")
	case "gemini":
		fmt.Fprintln(out, "  Get a free Gemini API key: https://aistudio.google.com/apikey")
		fmt.Fprintln(out, "")
	case "openai":
		fmt.Fprintln(out, "  Get an OpenAI API key: https://platform.openai.com/api-keys")
		fmt.Fprintln(out, "")
	}
	fmt.Fprintf(out, "  %d. Chat with your model:\n", map[bool]int{true: 2, false: 3}[provider != "ollama"])
	fmt.Fprintln(out, "       contenox hey, what can you do?")
	fmt.Fprintln(out, "       echo 'fix the typos in README.md' | contenox")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Plan and execute a multi-step task:")
	fmt.Fprintln(out, "       contenox plan new \"create a TODOS.md from all TODO comments in the codebase\"")
	fmt.Fprintln(out, "       contenox plan next --auto")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  To enable shell and filesystem tools pass --shell to any command, e.g.:")
	fmt.Fprintln(out, "       contenox --shell \"run the tests\"")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Run 'contenox --help' for full usage.")
	return nil
}
