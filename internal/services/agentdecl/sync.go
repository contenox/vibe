package agentdecl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GeneratedDirName holds transpiled chains. Everything in it is derived and is
// replaced whenever its source changes; the declaration is what an operator edits.
const GeneratedDirName = ".generated"

// SyncStateFilename maps each source to what it produced, so a pass can skip
// unchanged sources and retire chains whose source is gone.
const SyncStateFilename = ".sync-state.json"

// NativeSourceDir is where agents are authored inside a contenox directory, as
// Markdown with YAML frontmatter rather than chain JSON.
const NativeSourceDir = "agents"

// ForeignSourceDirs are where other tools keep the same files, relative to a
// workspace root.
var ForeignSourceDirs = []string{
	filepath.Join(".claude", "agents"),
	filepath.Join(".agents", "agents"),
}

// SourceDir is one directory of declarations. Native directories keep their own
// names; a foreign directory's are scoped by the product they came from.
type SourceDir struct {
	Path   string
	Native bool
}

// SyncResult is what one pass did with one source file.
type SyncResult struct {
	Source   string
	Name     string
	Dialect  Dialect
	Action   WriteAction
	Reason   string
	Unmapped []Unmapped
	// MCP and Remote are the tool sources this declaration brought with it, for
	// the caller to register scoped to Name. Returned rather than registered
	// here because registration needs a database this package does not have.
	MCP    []DeclaredMCPServer
	Remote []DeclaredRemoteTool
}

type syncRecord struct {
	SourcePath   string `json:"source_path"`
	SourceSHA256 string `json:"source_sha256"`
	ChainFile    string `json:"chain_file"`
	PolicyFile   string `json:"policy_file"`
}

// DiscoverSourceDirs returns the agent directories that exist, native ones
// first so a workspace's own declarations precede those of other tools.
func DiscoverSourceDirs(contenoxDirs []string, workspaceRoots []string) []SourceDir {
	var found []SourceDir
	seen := map[string]bool{}
	add := func(dir string, native bool) {
		if seen[dir] {
			return
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			seen[dir] = true
			found = append(found, SourceDir{Path: dir, Native: native})
		}
	}
	for _, dir := range contenoxDirs {
		if dir != "" {
			add(filepath.Join(dir, NativeSourceDir), true)
		}
	}
	for _, root := range workspaceRoots {
		if root == "" {
			continue
		}
		for _, rel := range ForeignSourceDirs {
			add(filepath.Join(root, rel), false)
		}
	}
	return found
}

// SyncOption tunes one pass. Variadic so a caller that has nothing to add —
// every test, and any caller predating a later option — stays unchanged.
type SyncOption func(*syncOptions)

type syncOptions struct {
	skills []Skill
}

// WithSkills supplies the procedures {{skills}} expands to. Omitted, the macro
// expands to "No skills are available." rather than being left in the prompt as
// a literal the model would have to interpret.
func WithSkills(skills []Skill) SyncOption {
	return func(o *syncOptions) { o.skills = skills }
}

// Sync transpiles every declaration under sourceDirs into generatedDir and
// retires chains whose source is gone. An unreadable or unmappable source is
// reported and skipped rather than failing the pass.
func Sync(sourceDirs []SourceDir, generatedDir string, cfg Config, opts ...SyncOption) ([]SyncResult, error) {
	var options syncOptions
	for _, opt := range opts {
		opt(&options)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	sources, err := collectSources(sourceDirs)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, retireAll(generatedDir)
	}
	if err := os.MkdirAll(generatedDir, 0o750); err != nil {
		return nil, fmt.Errorf("agentdecl: create %s: %w", generatedDir, err)
	}

	state := readSyncState(generatedDir)
	next := map[string]syncRecord{}
	results := make([]SyncResult, 0, len(sources))
	declared := map[string]bool{}

	for _, src := range sources {
		path := src.path
		res := SyncResult{Source: path}
		var ir *AgentIR
		var tree *AgentTree
		var err error
		if src.tree {
			tree, err = LoadTree(path, cfg)
			if err == nil {
				ir = tree.MergedIR()
			}
		} else {
			var data []byte
			data, err = os.ReadFile(path)
			if err == nil {
				// A native declaration is the format itself, not a file to identify.
				if src.native {
					ir, err = ParseClaudeCode(path, data, cfg)
				} else {
					ir, err = Parse(path, data, cfg)
				}
			}
		}
		if err != nil {
			res.Action, res.Reason = ActionRefused, err.Error()
			results = append(results, res)
			continue
		}
		res.Dialect, res.Unmapped = ir.Source.Dialect, ir.Unmapped
		// Expanded at generation, not per request.
		ir.SystemPrompt = expandSkills(ir.SystemPrompt, options.skills)

		agentCfg := cfg
		if src.native {
			agentCfg.Naming.ScopeWithDialect = false
		}
		res.Name = ir.ScopedName(agentCfg.Naming.ScopeWithDialect)
		res.MCP, res.Remote = ir.DeclaredMCP, ir.DeclaredRemote
		declared[res.Name] = true
		agentCfg, err = agentCfg.For(res.Name)
		if err != nil {
			res.Action, res.Reason = ActionRefused, err.Error()
			results = append(results, res)
			continue
		}
		var chain *taskengine.TaskChainDefinition
		if src.tree {
			chain, err = EmitTree(tree, agentCfg)
		} else {
			chain, err = EmitChain(ir, agentCfg)
		}
		if err != nil {
			res.Action, res.Reason = ActionRefused, err.Error()
			results = append(results, res)
			continue
		}
		policy, err := EmitPolicy(ir, agentCfg)
		if err != nil {
			res.Action, res.Reason = ActionRefused, err.Error()
			results = append(results, res)
			continue
		}
		res.Name = chain.ID
		if ReservedNames[chain.ID] {
			res.Action = ActionRefused
			res.Reason = fmt.Sprintf("%q is a shipped agent; a source file may not take its name", chain.ID)
			results = append(results, res)
			continue
		}

		// The generated filename keeps the chain-agent- prefix because
		// chainagents.eligible reads the basename, not the id, so the stem drops
		// the prefix the id already carries.
		stem := strings.TrimPrefix(chain.ID, "chain-")
		rec := syncRecord{
			SourcePath:   path,
			SourceSHA256: ir.Source.SHA256,
			ChainFile:    "chain-agent-" + stem + ".json",
			PolicyFile:   PolicyFileFor(stem),
		}
		// One namespace: an envelope of this name renders the same filename, and
		// the envelope owns it. The declaration still compiles; what it cannot
		// carry is its own posture, which is reported rather than overwritten.
		if _, taken := cfg.Envelopes[stem]; taken {
			rec.PolicyFile = ""
			res.Unmapped = append(res.Unmapped, Unmapped{
				Field: "posture",
				Reason: fmt.Sprintf("%q is also an envelope in %s, which owns %s; this agent runs under the envelope",
					stem, ConfigFilename, PolicyFileFor(stem)),
			})
		}
		chainPath := filepath.Join(generatedDir, rec.ChainFile)

		chainJSON, err := marshalWithSchema(chain, ChainSchemaURL)
		if err != nil {
			return nil, err
		}
		policyJSON, err := marshalWithSchema(policy, PolicySchemaURL)
		if err != nil {
			return nil, err
		}

		policyPath := ""
		policyCurrent := true
		if rec.PolicyFile != "" {
			policyPath = filepath.Join(generatedDir, rec.PolicyFile)
			policyCurrent = fileHas(policyPath, policyJSON)
		}
		// Compared against what is on disk, not the source hash alone: agents.toml
		// is the other half of the input.
		if _, ok := state[path]; ok && fileHas(chainPath, chainJSON) && policyCurrent {
			res.Action = ActionUnchanged
			next[path] = rec
			results = append(results, res)
			continue
		}

		if err := os.WriteFile(chainPath, chainJSON, 0o644); err != nil {
			return nil, fmt.Errorf("agentdecl: write %s: %w", chainPath, err)
		}
		if policyPath != "" {
			if err := os.WriteFile(policyPath, policyJSON, 0o644); err != nil {
				return nil, fmt.Errorf("agentdecl: write %s: %w", policyPath, err)
			}
		}

		if _, had := state[path]; had {
			res.Action = ActionUpdated
		} else {
			res.Action = ActionCreated
		}
		next[path] = rec
		results = append(results, res)
	}

	for src, rec := range state {
		if _, kept := next[src]; kept {
			continue
		}
		_ = os.Remove(filepath.Join(generatedDir, rec.ChainFile))
		if rec.PolicyFile != "" {
			_ = os.Remove(filepath.Join(generatedDir, rec.PolicyFile))
		}
	}
	for _, name := range cfg.UnknownAgents(declared) {
		results = append(results, SyncResult{
			Source: fmt.Sprintf("%s [agents.%s]", ConfigFilename, name),
			Name:   name,
			Action: ActionIgnored,
			Reason: "no declaration by that name; the section had no effect",
		})
	}
	if err := writeSyncState(generatedDir, next); err != nil {
		return results, err
	}
	return results, nil
}

func fileHas(path string, want []byte) bool {
	got, err := os.ReadFile(path)
	return err == nil && bytes.Equal(got, want)
}

func retireAll(generatedDir string) error {
	state := readSyncState(generatedDir)
	for _, rec := range state {
		_ = os.Remove(filepath.Join(generatedDir, rec.ChainFile))
		if rec.PolicyFile != "" {
			_ = os.Remove(filepath.Join(generatedDir, rec.PolicyFile))
		}
	}
	if len(state) == 0 {
		return nil
	}
	return writeSyncState(generatedDir, map[string]syncRecord{})
}

type sourceFile struct {
	path   string
	native bool
	// tree marks a DIRECTORY that holds an agent.md: one chain for the whole
	// subtree, rather than one per .md inside it.
	tree bool
}

func collectSources(dirs []SourceDir) ([]sourceFile, error) {
	var found []sourceFile
	for _, dir := range dirs {
		native := dir.Native
		err := filepath.WalkDir(dir.Path, func(p string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			// A directory holding an agent.md is a tree: the whole subtree is one
			// chain, so it is collected here and not descended into.
			if entry.IsDir() {
				if _, err := os.Stat(filepath.Join(p, AgentFilename)); err == nil {
					found = append(found, sourceFile{path: p, native: native, tree: true})
					return fs.SkipDir
				}
				return nil
			}
			if !strings.EqualFold(filepath.Ext(p), ".md") {
				return nil
			}
			// A README beside the declarations is documentation, not a declaration.
			if strings.EqualFold(entry.Name(), "README.md") {
				return nil
			}
			found = append(found, sourceFile{path: p, native: native})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("agentdecl: scan %s: %w", dir.Path, err)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].path < found[j].path })
	return found, nil
}

func readSyncState(dir string) map[string]syncRecord {
	state := map[string]syncRecord{}
	raw, err := os.ReadFile(filepath.Join(dir, SyncStateFilename))
	if err != nil {
		return state
	}
	_ = json.Unmarshal(raw, &state)
	return state
}

func writeSyncState(dir string, state map[string]syncRecord) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("agentdecl: marshal sync state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, SyncStateFilename), raw, 0o644); err != nil {
		return fmt.Errorf("agentdecl: write sync state: %w", err)
	}
	return nil
}
