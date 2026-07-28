package contenoxcli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

func loadChainFromFile(path string) (*taskengine.TaskChainDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read chain file %q: %w", path, err)
	}
	var chain taskengine.TaskChainDefinition
	if err := json.Unmarshal(data, &chain); err != nil {
		return nil, fmt.Errorf("failed to parse chain JSON %q: %w", path, err)
	}
	return &chain, nil
}
