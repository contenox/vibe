package contenoxcli

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/models/backendservice"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/project"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libtracker"
)

//go:embed chain-compact-default.json
var initCompactChain string

//go:embed chain-fim-default.json
var initFIMChain string

//go:embed chain-planner-default.json
var initPlannerChain string

//go:embed chain-oracle-default.json
var initOracleDefaultChain string

const (
	chainAgentACPFilename       = "chain-agent-acp.json"
	chainAgentACPXFilename      = "chain-agent-acpx.json"
	chainFIMDefaultFilename     = "chain-fim-default.json"
	chainCompactDefaultFilename = "chain-compact-default.json"
	chainPlannerDefaultFilename = "chain-planner-default.json"
	chainOracleDefaultFilename  = "chain-oracle-default.json"
)

// SystemDirName holds the shipped chain files — the runtime's own execution
// paths for chat, editor sessions and one-shot runs. They are machinery, not
// files an operator is expected to author, and a directory of them at the top
// level was the first thing `contenox init` said about the product.
//
// Loaders still read a same-named file in the workspace or directly in
// ~/.contenox/ first, so copying one up a level is all it takes to own it.
const SystemDirName = acpsvc.SystemDirName

// systemDir is where a contenox directory keeps its shipped chains.
func systemDir(contenoxDir string) string {
	return filepath.Join(contenoxDir, SystemDirName)
}

var blessedChainHashes = map[string][]string{}

var legacyChainRenames = map[string]string{
	"default-acp-chain.json":  chainAgentACPFilename,
	"headless-acp-chain.json": chainAgentACPXFilename,
	"default-fim-chain.json":  chainFIMDefaultFilename,
	"chain-compact.json":      chainCompactDefaultFilename,
	"agent-planner.json":      chainPlannerDefaultFilename,
}

func migrateLegacyChainNames(out io.Writer, dir string) error {
	if dir == "" {
		return nil
	}
	legacy := make([]string, 0, len(legacyChainRenames))
	for name := range legacyChainRenames {
		legacy = append(legacy, name)
	}
	sort.Strings(legacy)
	for _, name := range legacy {
		oldPath := filepath.Join(dir, name)
		if _, err := os.Stat(oldPath); err != nil {
			continue
		}
		newPath := filepath.Join(dir, legacyChainRenames[name])
		if _, err := os.Stat(newPath); err == nil {
			fmt.Fprintf(out, "  Kept %s; legacy %s left in place (both exist — merge or remove the legacy file yourself)\n", newPath, oldPath)
			continue
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return fmt.Errorf("rename %s to %s: %w", oldPath, newPath, err)
		}
		fmt.Fprintf(out, "  Renamed %s -> %s\n", oldPath, newPath)
	}
	return nil
}

func migrateLegacyChainNamesOnSearchPath(out io.Writer, contenoxDir string) error {
	homeDir, err := globalContenoxDir()
	if err != nil {
		return fmt.Errorf("could not resolve ~/.contenox: %w", err)
	}
	if err := migrateLegacyChainNames(out, homeDir); err != nil {
		return err
	}
	if contenoxDir != "" && contenoxDir != homeDir {
		return migrateLegacyChainNames(out, contenoxDir)
	}
	return nil
}

// migrateChainsIntoSystemDir relocates shipped chain files an earlier version
// wrote to the top level of contenoxDir.
//
// Only a file still byte-identical to what we shipped is moved: anything else
// is the operator's, and their copy at the top level keeps winning over the
// system one by the ordinary lookup order. So a customised chain survives an
// upgrade untouched, and an untouched one stops being clutter.
func migrateChainsIntoSystemDir(out io.Writer, contenoxDir string) error {
	if contenoxDir == "" {
		return nil
	}
	var moved []string
	for _, f := range initChainFiles {
		src := filepath.Join(contenoxDir, f.Name)
		onDisk, err := os.ReadFile(src)
		if err != nil || string(onDisk) != f.Content {
			continue
		}
		dir := systemDir(contenoxDir)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, f.Name), onDisk, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Join(dir, f.Name), err)
		}
		if err := os.Remove(src); err != nil {
			return fmt.Errorf("remove %s: %w", src, err)
		}
		moved = append(moved, f.Name)
	}
	if len(moved) > 0 {
		fmt.Fprintf(out, "  Moved %d unmodified chain file(s) into %s%c\n", len(moved), systemDir(contenoxDir), filepath.Separator)
	}
	return nil
}

func seedFIMChainIfMissing(contenoxDir string) error {
	if _, err := os.Stat(filepath.Join(contenoxDir, chainFIMDefaultFilename)); err == nil {
		return nil
	}
	dir := systemDir(contenoxDir)
	dst := filepath.Join(dir, chainFIMDefaultFilename)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(initFIMChain), 0644)
}

// initChainFiles are the chains still shipped as JSON.
//
// ⚠ acp and acpx are NOT here: they are declarations under
// agents/, seeded by agentdecl.Preseed and transpiled into .generated on every
// discovery pass. Shipping both would be two sources for one agent, and the
// JSON would be the one nobody edits — which is what made the recommended
// authoring format look optional.
//
// The four that remain are the ones the declaration format does not describe:
// compact and fim are single-task chains with no tool loop, and planner and
// oracle carry stages (a settle check, an early exit) with no counterpart in a
// declaration. Converting them would mean inventing behaviour, so they stay.
var initChainFiles = []struct {
	Name    string
	Content string
}{
	{chainCompactDefaultFilename, initCompactChain},
	{chainFIMDefaultFilename, initFIMChain},
	{chainPlannerDefaultFilename, initPlannerChain},
	{chainOracleDefaultFilename, initOracleDefaultChain},
}

var initTriggerFiles = []struct {
	Name    string
	Content string
}{}

func initSystemFileNames() []string {
	names := make([]string, 0, len(initChainFiles)+len(initTriggerFiles)+len(HITLPolicyPresets))
	for _, f := range initChainFiles {
		names = append(names, f.Name)
	}
	for _, f := range initTriggerFiles {
		names = append(names, f.Name)
	}
	for _, p := range HITLPolicyPresets {
		names = append(names, p.Name)
	}
	return names
}

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
		defaultModel: "gemini-flash-latest",
		envKey:       "GEMINI_API_KEY",
	},
	"openai": {
		name:         "OpenAI",
		defaultModel: "gpt-5-mini",
		envKey:       "OPENAI_API_KEY",
	},
	"anthropic": {
		name:         "Anthropic (direct)",
		defaultModel: "claude-sonnet-4-5",
		envKey:       "ANTHROPIC_API_KEY",
	},
	"bedrock": {
		name:         "AWS Bedrock",
		defaultModel: "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		envKey:       "", // ambient AWS credential chain
	},
	"vertex-google": {
		name:         "Google Vertex AI (Gemini)",
		defaultModel: "gemini-3.6-flash",
		envKey:       "",
	},
}

func hasBackendOfType(providerType string) bool {
	dbPath, err := globalDBPath()
	if err != nil {
		return false
	}
	db, err := OpenDBAt(libtracker.WithNewRequestID(context.Background()), dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	svc := backendservice.New(db)
	backends, err := svc.List(libtracker.WithNewRequestID(context.Background()), nil, 100)
	if err != nil {
		return false
	}
	for _, b := range backends {
		if strings.EqualFold(b.Type, providerType) {
			return true
		}
	}
	return false
}

// RunGlobalInit ensures ~/.contenox/ has chain files and HITL policies, without creating a workspace-scoped .contenox/ directory.
func RunGlobalInit(out io.Writer) error {
	homeDir, err := globalContenoxDir()
	if err != nil {
		return fmt.Errorf("could not resolve ~/.contenox: %w", err)
	}
	if err := os.MkdirAll(homeDir, 0o750); err != nil {
		return fmt.Errorf("create ~/.contenox: %w", err)
	}
	if err := migrateChainsIntoSystemDir(out, homeDir); err != nil {
		return err
	}
	sysDir := systemDir(homeDir)
	if err := os.MkdirAll(sysDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", sysDir, err)
	}
	writeFile := func(path, content string) error {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		fmt.Fprintf(out, "  Created %s\n", path)
		return nil
	}
	for _, f := range initChainFiles {
		// Skipped when the operator already owns a copy a level up, so a
		// customised chain is never shadowed by a fresh shipped one.
		if _, err := os.Stat(filepath.Join(homeDir, f.Name)); err == nil {
			continue
		}
		if err := writeFile(filepath.Join(sysDir, f.Name), f.Content); err != nil {
			return err
		}
	}
	if err := writeEmbeddedHITLPolicies(homeDir, false); err != nil {
		return err
	}
	if _, err := agentdecl.Preseed(homeDir); err != nil {
		return err
	}
	// Same reason as RunInit: the seeded declarations ARE the shipped agents, so
	// they have to be chains before anything looks one up.
	transpileSeededAgents(io.Discard, homeDir)
	return nil
}

func writeInitFile(out io.Writer, force, update bool, path, content string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		fmt.Fprintf(out, "  Created %s\n", path)
		return nil
	}

	if force {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		fmt.Fprintf(out, "  Overwrote %s (--force)\n", path)
		return nil
	}

	if update {
		basename := filepath.Base(path)
		if knownHashes, ok := blessedChainHashes[basename]; ok {
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("failed to read %s for update check: %w", path, err)
			}
			hash := sha256.Sum256(data)
			currentHash := hex.EncodeToString(hash[:])

			for _, knownHash := range knownHashes {
				if currentHash == knownHash {
					if err := os.WriteFile(path, []byte(content), 0644); err != nil {
						return fmt.Errorf("failed to write %s: %w", path, err)
					}
					fmt.Fprintf(out, "  Updated %s\n", path)
					return nil
				}
			}
		}
		fmt.Fprintf(out, "  Skipped %s (has been modified)\n", path)
		return nil
	}

	fmt.Fprintf(out, "  %s already exists (use --force to overwrite or --update to refresh)\n", path)
	return nil
}

// RunLocalInit seeds contenoxDir with the same chain files and HITL policy presets RunInit writes to ~/.contenox, as workspace-local overrides that shadow the global copies.
func RunLocalInit(out io.Writer, force, update bool, contenoxDir, projectName string) error {
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return fmt.Errorf("failed to create .contenox directory: %w", err)
	}
	marker, err := project.EnsureInContenoxDir(contenoxDir, projectName)
	if err != nil {
		return fmt.Errorf("failed to write project marker: %w", err)
	}
	if marker.Name != "" {
		fmt.Fprintf(out, "Marked project %q (%s)\n", marker.Name, filepath.Join(contenoxDir, project.MarkerFileName))
	}
	if update {
		if err := migrateLegacyChainNamesOnSearchPath(out, contenoxDir); err != nil {
			return err
		}
	}
	for _, f := range initChainFiles {
		if err := writeInitFile(out, force, update, filepath.Join(contenoxDir, f.Name), f.Content); err != nil {
			return err
		}
	}
	for _, f := range initTriggerFiles {
		if err := writeInitFile(out, force, update, filepath.Join(contenoxDir, f.Name), f.Content); err != nil {
			return err
		}
	}
	kept, err := upgradeEmbeddedHITLPolicies(contenoxDir, force)
	if err != nil {
		return err
	}
	seeded, err := agentdecl.Preseed(contenoxDir)
	if err != nil {
		return err
	}
	if problems := transpileSeededAgents(out, contenoxDir); problems != nil {
		printSyncProblems(out, problems)
	}
	for _, path := range seeded {
		fmt.Fprintf(out, "  Created %s\n", path)
	}
	keptSet := map[string]bool{}
	for _, name := range kept {
		keptSet[name] = true
	}
	for _, p := range HITLPolicyPresets {
		path := filepath.Join(contenoxDir, p.Name)
		if keptSet[p.Name] {
			fmt.Fprintf(out, "  Kept %s (has been modified; use --force to overwrite)\n", path)
			continue
		}
		fmt.Fprintf(out, "  Wrote %s\n", path)
	}
	fmt.Fprintln(out, "Done.")
	fmt.Fprintf(out, "These workspace copies shadow the presets in ~/.contenox — the workspace file wins by name.\n")
	return nil
}

// RunInit scaffolds contenoxDir with default chain files for provider ("" defaults to the configured provider or "ollama") and, if projectName is non-empty, renames the project marker.
func RunInit(out, errOut io.Writer, force, update bool, provider string, contenoxDir string, projectName string) error {
	provider = modelrepo.CanonicalBackendType(provider)
	if provider == "" {
		if dbPath, gpErr := globalDBPath(); gpErr == nil {
			if db, openErr := OpenDBAt(libtracker.WithNewRequestID(context.Background()), dbPath); openErr == nil {
				store := runtimetypes.New(db.WithoutTransaction())
				if cur, err := getConfigKV(libtracker.WithNewRequestID(context.Background()), store, "default-provider"); err == nil && cur != "" {
					cur = modelrepo.CanonicalBackendType(cur)
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
		return fmt.Errorf("unknown provider %q — valid options: ollama, openai, gemini, anthropic, bedrock, vertex-google", provider)
	}
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return fmt.Errorf("failed to create .contenox directory: %w", err)
	}
	// A fresh marker gets a new UUID; an existing one keeps its ID (an
	// explicit projectName renames it, "" leaves the stored name alone).
	marker, err := project.EnsureInContenoxDir(contenoxDir, projectName)
	if err != nil {
		return fmt.Errorf("failed to write project marker: %w", err)
	}
	if marker.Name != "" {
		fmt.Fprintf(out, "Marked project %q (%s)\n", marker.Name, filepath.Join(contenoxDir, project.MarkerFileName))
	}
	homeDir, hdErr := globalContenoxDir()
	if hdErr != nil {
		return fmt.Errorf("could not resolve ~/.contenox: %w", hdErr)
	}
	// Renames legacy-named files first so a blessed pre-rename file refreshes under its new name.
	if update {
		if err := migrateLegacyChainNamesOnSearchPath(out, contenoxDir); err != nil {
			return err
		}
	}
	// Loaders resolve workspace-first, so a same-named file in contenoxDir wins over the home copies written here; plain init never overwrites workspace files.
	noteShadowed := func(name string) {
		if contenoxDir == homeDir {
			return
		}
		wsPath := filepath.Join(contenoxDir, name)
		if _, err := os.Stat(wsPath); err == nil {
			fmt.Fprintf(out, "  note: %s shadows this file and was not updated\n", wsPath)
		}
	}
	writeHomeFile := func(path, content string) error {
		return writeInitFile(out, force, update, path, content)
	}
	writeFile := func(path, content string) error {
		if err := writeHomeFile(path, content); err != nil {
			return err
		}
		noteShadowed(filepath.Base(path))
		return nil
	}

	if err := migrateChainsIntoSystemDir(out, homeDir); err != nil {
		return err
	}
	homeSystemDir := systemDir(homeDir)
	if err := os.MkdirAll(homeSystemDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", homeSystemDir, err)
	}
	for _, f := range initChainFiles {
		// A copy the operator keeps a level up wins at lookup, so refreshing
		// the system copy underneath it would be invisible and confusing.
		if _, err := os.Stat(filepath.Join(homeDir, f.Name)); err == nil {
			fmt.Fprintf(out, "  note: %s is yours and still wins; %s not written\n",
				filepath.Join(homeDir, f.Name), filepath.Join(homeSystemDir, f.Name))
			continue
		}
		if err := writeFile(filepath.Join(homeSystemDir, f.Name), f.Content); err != nil {
			return err
		}
	}
	if force {
		if err := refreshPoliciesOnSearchPath(out, contenoxDir); err != nil {
			return err
		}
	} else {
		if err := writeEmbeddedHITLPolicies(homeDir, false); err != nil {
			return err
		}
		for _, p := range HITLPolicyPresets {
			noteShadowed(p.Name)
		}
	}
	if _, err := agentdecl.Preseed(homeDir); err != nil {
		return err
	}
	// The seeded declarations become chains here rather than at first use, so a
	// fresh install has a working acp before anything asks for
	// one. Reported, not fatal: a declaration that will not transpile is worth
	// naming, and the rest of init still stands.
	if problems := transpileSeededAgents(out, homeDir); problems != nil {
		printSyncProblems(out, problems)
	}

	fmt.Fprintln(out, "Done.")
	fmt.Fprintln(out, "")

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
	fmt.Fprintln(out, "  Prefer a guided path? 'contenox setup' registers a backend and sets the defaults for you.")
	fmt.Fprintln(out, "")
	chatStep := 3
	switch provider {
	case "vertex-google":
		fmt.Fprintln(out, "  1. Authenticate with Google Cloud:")
		fmt.Fprintln(out, "       export GOOGLE_CLOUD_PROJECT=my-project-id")
		fmt.Fprintln(out, "       gcloud auth application-default login --project $GOOGLE_CLOUD_PROJECT")
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "  2. Register the %s backend:\n", pc.name)
		fmt.Fprintf(out, "       contenox backend add %s --type %s \\\n", provider, provider)
		fmt.Fprintln(out, `         --url "https://aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/global"`)
		fmt.Fprintln(out, `       (or regional, for data residency: --url "https://{REGION}-aiplatform.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/locations/{REGION}")`)
		fmt.Fprintln(out, "       contenox doctor")
		fmt.Fprintln(out, "       contenox model list   # model availability differs per endpoint")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  3. Set defaults:")
		fmt.Fprintf(out, "       contenox config set default-provider %s\n", provider)
		fmt.Fprintf(out, "       contenox config set default-model %s\n", pc.defaultModel)
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  Get started with Vertex AI: https://cloud.google.com/vertex-ai/generative-ai/docs/start/quickstarts")
		fmt.Fprintln(out, "")
		chatStep = 4
	case "ollama":
		if base, ok := setupcheck.ProbeLocalOllamaAPI(context.Background()); ok {
			fmt.Fprintf(out, "  Local Ollama is already reachable at %s. Skip steps 1-2 on this machine if install, ollama serve, and ollama pull (e.g. qwen3:8b) are already done.\n\n", base)
		}
		fmt.Fprintln(out, "  1. Install Ollama (if not already):")
		fmt.Fprintln(out, "       curl -fsSL https://ollama.com/install.sh | sh")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  2. Run the Ollama server (leave it running), then pull a model in another terminal:")
		fmt.Fprintln(out, "       ollama serve")
		fmt.Fprintln(out, "       ollama pull qwen3:8b")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  3. Register the local API and set defaults (URLs match contenox backend add defaults):")
		fmt.Fprintln(out, "       contenox backend add ollama --type ollama")
		fmt.Fprintln(out, "       contenox config set default-provider ollama")
		fmt.Fprintln(out, "       contenox config set default-model qwen3:8b")
		fmt.Fprintln(out, "       contenox doctor")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  Optional: use hosted Ollama Cloud instead of a local server:")
		fmt.Fprintln(out, "       export OLLAMA_API_KEY=your-key-here")
		fmt.Fprintln(out, "       contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY")
		fmt.Fprintln(out, "  Get an Ollama API key for direct cloud access: https://ollama.com/settings/keys")
		fmt.Fprintln(out, "")
		chatStep = 4
	default:
		backendRegistered := hasBackendOfType(provider)
		registerStep := 1
		if !backendReady && pc.envKey != "" {
			fmt.Fprintf(out, "  1. Set your %s API key:\n", pc.name)
			fmt.Fprintf(out, "       export %s=your-key-here\n", pc.envKey)
			switch provider {
			case "gemini":
				fmt.Fprintln(out, "  Get a free Gemini API key: https://aistudio.google.com/apikey")
			case "openai":
				fmt.Fprintln(out, "  Get an OpenAI API key: https://platform.openai.com/api-keys")
			}
			fmt.Fprintln(out, "")
			registerStep = 2
		}
		if !backendRegistered {
			fmt.Fprintf(out, "  %d. Register the %s backend and set defaults:\n", registerStep, pc.name)
			if pc.envKey != "" {
				fmt.Fprintf(out, "       contenox backend add %s --type %s --api-key-env %s\n", provider, provider, pc.envKey)
			} else {
				fmt.Fprintf(out, "       contenox backend add %s --type %s --url \"https://bedrock-runtime.eu-central-1.amazonaws.com\"   # region lives in the URL; credentials come from the AWS chain\n", provider, provider)
			}
			fmt.Fprintf(out, "       contenox config set default-provider %s\n", provider)
			fmt.Fprintf(out, "       contenox config set default-model %s\n", pc.defaultModel)
			fmt.Fprintln(out, "       contenox doctor")
			fmt.Fprintln(out, "")
			chatStep = registerStep + 1
		} else {
			chatStep = registerStep
		}
	}
	fmt.Fprintf(out, "  %d. Chat with your model:\n", chatStep)
	fmt.Fprintln(out, "       contenox hey, what can you do?")
	fmt.Fprintln(out, "       echo 'fix the typos in README.md' | contenox")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  To enable shell and filesystem tools pass --shell to any command, e.g.:")
	fmt.Fprintln(out, "       contenox --shell \"run the tests\"")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Run 'contenox --help' for full usage.")
	return nil
}

// transpileSeededAgents turns the declarations under contenoxDir/agents into
// chains in .generated, and returns whatever refused.
func transpileSeededAgents(out io.Writer, contenoxDir string) []agentdecl.SyncResult {
	cfg, err := agentdecl.Load(contenoxDir)
	if err != nil {
		fmt.Fprintf(out, "  note: could not read %s (%v); declared agents were not transpiled\n",
			agentdecl.ConfigFilename, err)
		return nil
	}
	dirs := agentdecl.DiscoverSourceDirs([]string{contenoxDir}, nil)
	if len(dirs) == 0 {
		return nil
	}
	generated := filepath.Join(contenoxDir, agentdecl.GeneratedDirName)
	results, err := agentdecl.Sync(dirs, generated, cfg)
	if err != nil {
		fmt.Fprintf(out, "  note: transpiling declared agents failed (%v)\n", err)
		return nil
	}
	made := 0
	var problems []agentdecl.SyncResult
	for _, r := range results {
		if r.Action == agentdecl.ActionRefused {
			problems = append(problems, r)
			continue
		}
		made++
	}
	if made > 0 {
		fmt.Fprintf(out, "  Transpiled %d declared agent(s) into %s%c\n", made, generated, filepath.Separator)
	}
	return problems
}
