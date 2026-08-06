// init.go implements the contenox init subcommand (scaffold .contenox/).
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
	"github.com/contenox/contenox/internal/services/project"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
)

//go:embed chain-agent-contenox.json
var initChain string

//go:embed chain-agent-run.json
var initRunChain string

//go:embed chain-compact-default.json
var initCompactChain string

//go:embed chain-agent-acp.json
var initACPChain string

//go:embed chain-agent-acpx.json
var initACPXChain string

//go:embed chain-agent-beam.json
var initBeamChain string

//go:embed chain-fim-default.json
var initFIMChain string

//go:embed chain-planner-default.json
var initPlannerChain string

//go:embed chain-oracle-default.json
var initOracleDefaultChain string

//go:embed chain-oracle-conservative.json
var initOracleConservativeChain string

// Seeded chain-file basenames, chain-<role>-<variant>.json. One name
// everywhere: the embedded source, the seeded disk file, and the docs all use
// the same string.
const (
	chainAgentContenoxFilename      = "chain-agent-contenox.json"
	chainAgentRunFilename           = "chain-agent-run.json"
	chainAgentACPFilename           = "chain-agent-acp.json"
	chainAgentACPXFilename          = "chain-agent-acpx.json"
	chainAgentBeamFilename          = "chain-agent-beam.json"
	chainFIMDefaultFilename         = "chain-fim-default.json"
	chainCompactDefaultFilename     = "chain-compact-default.json"
	chainPlannerDefaultFilename     = "chain-planner-default.json"
	chainOracleDefaultFilename      = "chain-oracle-default.json"
	chainOracleConservativeFilename = "chain-oracle-conservative.json"
)

// blessedChainHashes maps CURRENT seeded basenames to known-good SHA256
// checksums from previous builds; --update overwrites a file whose checksum
// matches. The --update rename migration (migrateLegacyChainNames) runs first,
// so a blessed pre-rename file is refreshed under its new name.
var blessedChainHashes = map[string][]string{}

// legacyChainRenames maps every pre-convention seeded basename to its
// chain-<role>-<variant>.json successor. `contenox init --update` renames
// these on disk (see migrateLegacyChainNames); resolution never consults the
// legacy names. User-authored files are outside this map and never touched.
var legacyChainRenames = map[string]string{
	"default-chain.json":      chainAgentContenoxFilename,
	"default-run-chain.json":  chainAgentRunFilename,
	"default-acp-chain.json":  chainAgentACPFilename,
	"headless-acp-chain.json": chainAgentACPXFilename,
	"default-beam-chain.json": chainAgentBeamFilename,
	"default-fim-chain.json":  chainFIMDefaultFilename,
	"chain-compact.json":      chainCompactDefaultFilename,
	"agent-planner.json":      chainPlannerDefaultFilename,
}

// migrateLegacyChainNames renames the shipped legacy-named chain files in dir
// to their chain-<role>-<variant>.json names. A rename is byte-for-byte, never
// a rewrite, so hand-edited files survive; the normal --update checksum
// refresh then applies under the new name. When both names exist the new file
// wins and the legacy file is left in place with a one-line note. Idempotent.
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

// migrateLegacyChainNamesOnSearchPath runs the --update rename in ~/.contenox
// and, when distinct, the workspace contenoxDir: a workspace shadow copy left
// under its legacy name would silently stop shadowing its home counterpart.
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

// seedHeadlessACPChainIfMissing writes the embedded acpx chain to contenoxDir
// only when absent. It never overwrites a user-edited file, and a failure here
// leaves the file absent so LoadChainRegistryFrom still fails closed rather
// than the acpx profile silently running a different chain.
func seedHeadlessACPChainIfMissing(contenoxDir string) error {
	dst := filepath.Join(contenoxDir, chainAgentACPXFilename)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(initACPXChain), 0644)
}

// seedACPChainIfMissing writes the chain-agent-acp.json preset when it is
// absent, so the `acp` profile is self-sufficient on a fresh install, the
// same way `acpx` is via seedHeadlessACPChainIfMissing. Without this, a
// clean environment that never ran `contenox init`/`--setup` hard-errors at
// launch in LoadChainRegistryFrom.
func seedACPChainIfMissing(contenoxDir string) error {
	dst := filepath.Join(contenoxDir, chainAgentACPFilename)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(initACPChain), 0644)
}

// seedFIMChainIfMissing writes the chain-fim-default.json preset when it is
// absent, so `contenox acp` autocomplete (_contenox/autocomplete) works on a
// fresh install, the same self-sufficiency seedACPChainIfMissing gives the
// chat chain. Callers must treat a failure here as non-fatal: autocomplete
// is optional and a missing/unwritable FIM chain must not block `acp`
// startup (see loadOptionalFIMChain / acpsvc's nil FIMChainRegistry check).
func seedFIMChainIfMissing(contenoxDir string) error {
	dst := filepath.Join(contenoxDir, chainFIMDefaultFilename)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(initFIMChain), 0644)
}

// seedBeamChainIfMissing writes the embedded beam chain to contenoxDir only
// when absent, the same self-sufficiency seedACPChainIfMissing gives the
// editor profile: a clean environment that never ran `contenox init` still
// gets a working `contenox beam` rather than a hard-error in LoadChainRegistryFrom.
func seedBeamChainIfMissing(contenoxDir string) error {
	dst := filepath.Join(contenoxDir, chainAgentBeamFilename)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(initBeamChain), 0644)
}

// initChainFiles pairs every chain-file basename init seeds with its embedded
// content, in write order. RunInit/RunGlobalInit keep their explicit write
// calls; this list backs RunLocalInit and the doctor shadow report.
var initChainFiles = []struct {
	Name    string
	Content string
}{
	{chainAgentContenoxFilename, initChain},
	{chainAgentRunFilename, initRunChain},
	{chainCompactDefaultFilename, initCompactChain},
	{chainAgentACPFilename, initACPChain},
	{chainFIMDefaultFilename, initFIMChain},
	{chainAgentACPXFilename, initACPXChain},
	{chainAgentBeamFilename, initBeamChain},
	{chainPlannerDefaultFilename, initPlannerChain},
	{chainOracleDefaultFilename, initOracleDefaultChain},
	{chainOracleConservativeFilename, initOracleConservativeChain},
}

// initTriggerFiles pairs every trigger-file basename init seeds with its
// embedded content, mirroring initChainFiles. Currently empty: the generic
// trigger tier stays operator-authored (no seeded example trigger); the
// oracle no longer rides a trigger — `mission fire --oracle` mounts it as an
// in-process attention driver.
var initTriggerFiles = []struct {
	Name    string
	Content string
}{}

// initSystemFileNames returns every system-file basename init seeds: the chain
// files, the trigger files, then the HITL policy presets.
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

// hasBackendOfType returns true when the local DB already contains at least one
// backend whose Type matches the given provider string.
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

// RunGlobalInit ensures ~/.contenox/ has chain files and HITL policies.
// Unlike RunInit it does NOT create a workspace-scoped .contenox/ directory.
func RunGlobalInit(out io.Writer) error {
	homeDir, err := globalContenoxDir()
	if err != nil {
		return fmt.Errorf("could not resolve ~/.contenox: %w", err)
	}
	if err := os.MkdirAll(homeDir, 0o750); err != nil {
		return fmt.Errorf("create ~/.contenox: %w", err)
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
	if err := writeFile(filepath.Join(homeDir, chainAgentContenoxFilename), initChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainAgentRunFilename), initRunChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainCompactDefaultFilename), initCompactChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainAgentACPFilename), initACPChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainFIMDefaultFilename), initFIMChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainAgentACPXFilename), initACPXChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainAgentBeamFilename), initBeamChain); err != nil {
		return err
	}
	// Discovered as a fleet-dispatchable agent by its shipped chain id
	// (agent-planner); its envelope grants only mission tools.
	if err := writeFile(filepath.Join(homeDir, chainPlannerDefaultFilename), initPlannerChain); err != nil {
		return err
	}
	// Oracle attention-driver chains: inert until `mission fire --oracle`
	// (opt-in-beta) mounts the driver.
	if err := writeFile(filepath.Join(homeDir, chainOracleDefaultFilename), initOracleDefaultChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainOracleConservativeFilename), initOracleConservativeChain); err != nil {
		return err
	}
	if err := writeEmbeddedHITLPolicies(homeDir, false); err != nil {
		return err
	}
	return nil
}

// writeInitFile writes one init preset with init's flag semantics: create when
// absent, overwrite on force, on update overwrite only a file whose checksum
// matches a blessed prior build, otherwise leave the file and report it.
func writeInitFile(out io.Writer, force, update bool, path, content string) error {
	// If the file doesn't exist, we always write it.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		fmt.Fprintf(out, "  Created %s\n", path)
		return nil
	}

	// If we're forcing, we always overwrite.
	if force {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", path, err)
		}
		fmt.Fprintf(out, "  Overwrote %s (--force)\n", path)
		return nil
	}

	// If --update is passed, we check the checksum and overwrite if it's a known-good, unmodified file.
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
					// Checksum matches, safe to overwrite.
					if err := os.WriteFile(path, []byte(content), 0644); err != nil {
						return fmt.Errorf("failed to write %s: %w", path, err)
					}
					fmt.Fprintf(out, "  Updated %s\n", path)
					return nil
				}
			}
		}
		// If we're here, the file was either not in the blessed list or the checksum didn't match.
		// We don't overwrite it, but we also don't print a scary "already exists" message.
		fmt.Fprintf(out, "  Skipped %s (has been modified)\n", path)
		return nil
	}

	// Default case: file exists, no --force, no --update. Do nothing.
	fmt.Fprintf(out, "  %s already exists (use --force to overwrite or --update to refresh)\n", path)
	return nil
}

// RunLocalInit seeds the chain files and HITL policy presets RunInit writes to
// ~/.contenox into contenoxDir itself — deliberate workspace-local overrides
// that shadow the global copies via the loaders' workspace-first resolution.
// Chain files follow writeInitFile's --force/--update semantics; policy
// presets follow the provenance-tracked upgrade (hand-edited files are kept
// unless force).
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

// RunInit scaffolds .contenox/ with default chain files.
// provider is "" (defaults to the configured provider or "ollama") or one of providerConfigs.
// contenoxDir is the target data directory. projectName, if non-empty, renames
// an already-named project's marker; "" leaves the marker's name alone.
func RunInit(out, errOut io.Writer, force, update bool, provider string, contenoxDir string, projectName string) error {
	provider = modelrepo.CanonicalBackendType(provider)
	if provider == "" {
		// Default to the provider already configured in the database so that
		// re-running init doesn't show irrelevant setup steps.
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
	// The one-time convention migration: rename shipped legacy-named files to
	// their chain-<role>-<variant>.json names before the refresh below, so a
	// blessed pre-rename file is refreshed under its new name.
	if update {
		if err := migrateLegacyChainNamesOnSearchPath(out, contenoxDir); err != nil {
			return err
		}
	}
	// Loaders resolve workspace-first (lookupSystemFile, hitlPolicySource): a
	// same-named file in contenoxDir wins over the home copies written below.
	// Plain init never overwrites workspace files.
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

	if err := writeFile(filepath.Join(homeDir, chainAgentContenoxFilename), initChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainAgentRunFilename), initRunChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainCompactDefaultFilename), initCompactChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainAgentACPFilename), initACPChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainFIMDefaultFilename), initFIMChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainAgentACPXFilename), initACPXChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainAgentBeamFilename), initBeamChain); err != nil {
		return err
	}
	// Discovered as a fleet-dispatchable agent by its shipped chain id
	// (agent-planner); its envelope grants only mission tools.
	if err := writeFile(filepath.Join(homeDir, chainPlannerDefaultFilename), initPlannerChain); err != nil {
		return err
	}
	// Oracle attention-driver chains: inert until `mission fire --oracle`
	// (opt-in-beta) mounts the driver.
	if err := writeFile(filepath.Join(homeDir, chainOracleDefaultFilename), initOracleDefaultChain); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(homeDir, chainOracleConservativeFilename), initOracleConservativeChain); err != nil {
		return err
	}
	if force {
		// The same search-path refresh as `init --refresh-policies`: forcing
		// only the home copy would leave a workspace copy shadowing it.
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
