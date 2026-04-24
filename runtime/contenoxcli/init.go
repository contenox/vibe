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
	"github.com/contenox/contenox/runtime/internal/runtimestate"
	"github.com/contenox/contenox/runtime/internal/setupcheck"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/google/uuid"
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
	"local": {
		name:         "Local (GGUF)",
		defaultModel: "",
		envKey:       "",
	},
	"vertex-google": {
		name:         "Google Vertex AI (Gemini)",
		defaultModel: "gemini-2.5-flash-preview-04-17",
		envKey:       "",
	},
	"vertex-anthropic": {
		name:         "Google Vertex AI (Anthropic)",
		defaultModel: "claude-sonnet-4-5-20251029",
		envKey:       "",
	},
	"vertex-meta": {
		name:         "Google Vertex AI (Meta)",
		defaultModel: "llama-3.1-405b-instruct-maas",
		envKey:       "",
	},
	"vertex-mistralai": {
		name:         "Google Vertex AI (Mistral)",
		defaultModel: "mistral-large-2411",
		envKey:       "",
	},
}

// RunInit scaffolds .contenox/ with default chain files.
// provider is "" (defaults to the already-configured provider or "ollama"), "ollama", "gemini", "openai", or "local".
// contenoxDir is the target data directory (e.g. from --data-dir or the default .contenox/).
func RunInit(out, errOut io.Writer, force bool, provider string, contenoxDir string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		// Default to the provider already configured in the database so that
		// re-running init doesn't show irrelevant setup steps.
		if dbPath, gpErr := globalDBPath(); gpErr == nil {
			if db, openErr := OpenDBAt(libtracker.WithNewRequestID(context.Background()), dbPath); openErr == nil {
				store := runtimetypes.New(db.WithoutTransaction())
				if cur, err := getConfigKV(libtracker.WithNewRequestID(context.Background()), store, "default-provider"); err == nil && cur != "" {
					if _, known := providerConfigs[cur]; known {
						provider = cur
					}
				}
				db.Close()
			}
		}
		if provider == "" {
			provider = "ollama"
		}
	}

	pc, ok := providerConfigs[provider]
	if !ok {
		return fmt.Errorf("unknown provider %q — valid options: ollama, gemini, openai, local, vertex-google, vertex-anthropic, vertex-meta, vertex-mistralai", provider)
	}
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return fmt.Errorf("failed to create .contenox directory: %w", err)
	}
	wsPath := filepath.Join(contenoxDir, "workspace.id")
	if _, err := os.Stat(wsPath); os.IsNotExist(err) {
		_ = os.WriteFile(wsPath, []byte(uuid.NewString()), 0o644)
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

	if err := writeEmbeddedHITLPolicies(contenoxDir, force); err != nil {
		return err
	}

	plannerPath, executorPath, summarizerPath, wrotePlanner, wroteExecutor, wroteSummarizer, err := writeEmbeddedPlanChains(contenoxDir, force)
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
	if !wroteSummarizer {
		fmt.Fprintf(out, "  %s already exists (use --force to overwrite)\n", summarizerPath)
	} else {
		fmt.Fprintf(out, "  Created %s\n", summarizerPath)
	}
	fmt.Fprintln(out, "  Plan commands use these chains; running 'contenox plan new' or 'plan next' refreshes them from the binary.")
	fmt.Fprintln(out, "  After registering a backend, run 'contenox doctor' to verify setup before planning.")

	fmt.Fprintln(out, "Done.")
	fmt.Fprintln(out, "")

	// Surface the currently configured model so users immediately know
	// if they have a stale entry from a previous install.
	if dbPath, gpErr := globalDBPath(); gpErr == nil {
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
				fmt.Fprintln(out, "Current config (from ~/.contenox/local.db):")
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

	// Resolve API key status (env or KV store) — used both for the status line and to
	// suppress the "register backend" step when the backend is already configured.
	var envVal string
	var kvHasKey bool
	if pc.envKey != "" {
		envVal = os.Getenv(pc.envKey)
		if envVal == "" {
			if dbPath, gpErr := globalDBPath(); gpErr == nil {
				if db, openErr := OpenDBAt(libtracker.WithNewRequestID(context.Background()), dbPath); openErr == nil {
					store := runtimetypes.New(db.WithoutTransaction())
					var cfg runtimestate.ProviderConfig
					kvKey := runtimestate.ProviderKeyPrefix + strings.ToLower(provider)
					if err := store.GetKV(libtracker.WithNewRequestID(context.Background()), kvKey, &cfg); err == nil && cfg.APIKey != "" {
						kvHasKey = true
					}
					db.Close()
				}
			}
		}
		switch {
		case envVal != "":
			fmt.Fprintf(out, "✓  %s API key detected (%s).\n\n", pc.name, pc.envKey)
		case kvHasKey:
			fmt.Fprintf(out, "✓  %s API key stored in local.db (set %s to use a different key).\n\n", pc.name, pc.envKey)
		default:
			fmt.Fprintf(out, "⚠️  %s API key not found in environment.\n", pc.name)
			fmt.Fprintf(out, "   Set it before running contenox:\n\n")
			fmt.Fprintf(out, "     export %s=your-key-here\n\n", pc.envKey)
		}
	}
	backendReady := kvHasKey || envVal != ""

	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintln(out, "")
	chatStep := 3
	switch provider {
	case "vertex-google", "vertex-anthropic", "vertex-meta", "vertex-mistralai":
		fmt.Fprintln(out, "  1. Authenticate with Google Cloud:")
		fmt.Fprintln(out, "       export GOOGLE_CLOUD_PROJECT=my-project-id")
		fmt.Fprintln(out, "       gcloud auth application-default login --project $GOOGLE_CLOUD_PROJECT")
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "  2. Register the %s backend:\n", pc.name)
		fmt.Fprintf(out, "       contenox backend add %s --type %s \\\n", provider, provider)
		fmt.Fprintln(out, `         --url "https://us-central1-aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/us-central1"`)
		fmt.Fprintln(out, "       contenox doctor")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  3. Set defaults:")
		fmt.Fprintf(out, "       contenox config set default-provider %s\n", provider)
		fmt.Fprintf(out, "       contenox config set default-model %s\n", pc.defaultModel)
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  Get started with Vertex AI: https://cloud.google.com/vertex-ai/generative-ai/docs/start/quickstarts")
		fmt.Fprintln(out, "")
		chatStep = 4
	case "local":
		fmt.Fprintln(out, "  1. Pull a model (choose by available VRAM):")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "       VRAM     Model              Q4 size   Notes")
		fmt.Fprintln(out, "       ~2 GB    granite-3.2-2b     ~1-2 GB   good tool use")
		fmt.Fprintln(out, "       ~3 GB    qwen3-4b           ~3 GB")
		fmt.Fprintln(out, "       ~3 GB    gemma4-e2b         ~3.2 GB   (BF16: 9.6 GB, SFP8: 4.6 GB, Q4: 3.2 GB)")
		fmt.Fprintln(out, "       ~5 GB    gemma4-e4b         ~5 GB     (BF16: 15 GB, SFP8: 7.5 GB, Q4: 5 GB)")
		fmt.Fprintln(out, "       ~17 GB   gemma4-31b         ~17 GB    (BF16: 58.3 GB, SFP8: 30.4 GB)")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "       contenox model registry-list   # full list with sizes")
		fmt.Fprintln(out, "       contenox model pull granite-3.2-2b")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  2. Register the local backend and set defaults:")
		fmt.Fprintln(out, "       contenox backend add local --type local --url ~/.contenox/models/")
		fmt.Fprintln(out, "       contenox config set default-provider local")
		fmt.Fprintln(out, "       contenox config set default-model granite-3.2-2b")
		fmt.Fprintln(out, "       contenox doctor")
		fmt.Fprintln(out, "")
		chatStep = 3
	case "ollama":
		if base, ok := setupcheck.ProbeLocalOllamaAPI(context.Background()); ok {
			fmt.Fprintf(out, "  Local Ollama is already reachable at %s. Skip steps 1-2 on this machine if install, ollama serve, and ollama pull (e.g. qwen2.5:7b) are already done.\n\n", base)
		}
		fmt.Fprintln(out, "  1. Install Ollama (if not already):")
		fmt.Fprintln(out, "       curl -fsSL https://ollama.com/install.sh | sh")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  2. Run the Ollama server (leave it running), then pull a model in another terminal:")
		fmt.Fprintln(out, "       ollama serve")
		fmt.Fprintln(out, "       ollama pull qwen2.5:7b")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  3. Register the local API and set defaults (URLs match contenox backend add defaults):")
		fmt.Fprintln(out, "       contenox backend add ollama --type ollama")
		fmt.Fprintln(out, "       contenox config set default-provider ollama")
		fmt.Fprintln(out, "       contenox config set default-model qwen2.5:7b")
		fmt.Fprintln(out, "       contenox doctor")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  Optional: use hosted Ollama Cloud instead of a local server:")
		fmt.Fprintln(out, "       export OLLAMA_API_KEY=your-key-here")
		fmt.Fprintln(out, "       contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY")
		fmt.Fprintln(out, "  Get an Ollama API key for direct cloud access: https://ollama.com/settings/keys")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  Optional: run fully local models via the local provider:")
		fmt.Fprintln(out, "       contenox init local")
		fmt.Fprintln(out, "")
		chatStep = 4
	default:
		if !backendReady {
			fmt.Fprintf(out, "  1. Register the %s backend:\n", pc.name)
			fmt.Fprintf(out, "       contenox backend add %s --type %s --api-key-env %s\n", provider, provider, pc.envKey)
			fmt.Fprintf(out, "       contenox model list   # confirm the runtime can see %s models\n", pc.name)
			fmt.Fprintf(out, "       contenox config set default-model %s\n", pc.defaultModel)
			fmt.Fprintln(out, "")
			switch provider {
			case "gemini":
				fmt.Fprintln(out, "  Get a free Gemini API key: https://aistudio.google.com/apikey")
				fmt.Fprintln(out, "")
			case "openai":
				fmt.Fprintln(out, "  Get an OpenAI API key: https://platform.openai.com/api-keys")
				fmt.Fprintln(out, "")
			}
			chatStep = 3
		} else {
			chatStep = 1
		}
	}
	fmt.Fprintf(out, "  %d. Chat with your model:\n", chatStep)
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
