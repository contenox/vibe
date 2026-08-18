package acpsvc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentdecl"
)

// SystemDirName is the subdirectory of a contenox directory holding the shipped
// chain files.
const SystemDirName = "system"

const (
	defaultChainFilename = "chain-agent-acp.json"
	chainPathEnv         = "CONTENOX_ACP_CHAIN_PATH"

	defaultFIMChainFilename = "chain-fim-default.json"
	fimChainPathEnv         = "CONTENOX_ACP_FIM_CHAIN_PATH"
)

type ChainRegistry struct {
	defaultChain *taskengine.TaskChainDefinition
	source       string
}

func LoadChainRegistry() (*ChainRegistry, error) {
	return LoadChainRegistryFrom(defaultChainFilename, chainPathEnv)
}

// LoadChainRegistryFrom loads the ACP chain for a specific profile: filename is
// the ~/.contenox/ file the chain is read from, envVar overrides that path. A
// missing file is a hard error.
func LoadChainRegistryFrom(filename, envVar string) (*ChainRegistry, error) {
	path := os.Getenv(envVar)
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("acpsvc: cannot determine home directory and %s is not set: %w", envVar, err)
		}
		candidates := ChainSearchPath(filepath.Join(home, ".contenox"), filename)
		// Falls back to the last candidate so a total miss names the system copy.
		path = candidates[len(candidates)-1]
		for _, c := range candidates {
			if _, statErr := os.Stat(c); statErr == nil {
				path = c
				break
			}
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("acpsvc: chain file %q not found; populate it like any other contenox chain or set %s: %w", path, envVar, err)
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(data, &chain); err != nil {
		return nil, fmt.Errorf("acpsvc: invalid chain JSON at %q: %w", path, err)
	}
	if chain.ID == "" {
		return nil, fmt.Errorf("acpsvc: chain at %q has empty ID", path)
	}
	if len(chain.Tasks) == 0 {
		return nil, fmt.Errorf("acpsvc: chain at %q has no tasks", path)
	}
	return &ChainRegistry{defaultChain: &chain, source: path}, nil
}

// ChainSearchPath is where LoadChainRegistryFrom looks for filename under
// contenoxDir when the env override is unset, nearest first: an operator copy at
// the top level, then the compiled declarations, then the shipped system copy.
func ChainSearchPath(contenoxDir, filename string) []string {
	return []string{
		filepath.Join(contenoxDir, filename),
		filepath.Join(contenoxDir, agentdecl.GeneratedDirName, filename),
		filepath.Join(contenoxDir, SystemDirName, filename),
	}
}

// ChainFileResolves reports whether any ChainSearchPath candidate exists, which
// is what separates a contenox directory that can serve filename from one a
// caller still has to populate.
func ChainFileResolves(contenoxDir, filename string) bool {
	for _, p := range ChainSearchPath(contenoxDir, filename) {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func (r *ChainRegistry) Default() *taskengine.TaskChainDefinition { return r.defaultChain }

func (r *ChainRegistry) Source() string { return r.source }

// LoadFIMChainRegistry loads the fill-in-the-middle chain for
// _contenox/autocomplete, mirroring LoadChainRegistry's file and env-var
// convention. A missing or invalid file is a hard error.
func LoadFIMChainRegistry() (*ChainRegistry, error) {
	return LoadChainRegistryFrom(defaultFIMChainFilename, fimChainPathEnv)
}
