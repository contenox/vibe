package agentsmd

import (
	"os"
	"path/filepath"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
)

const (
	Filename        = "AGENTS.md"
	MaxBytes        = 64 * 1024
	TruncatedNotice = "\n\n[AGENTS.md truncated to 64 KiB; remove this limit by editing runtime/agentsmd.MaxBytes]"
)

func Load(startDir string) (string, string, bool) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", "", false
	}
	for {
		candidate := filepath.Join(dir, Filename)
		if data, err := os.ReadFile(candidate); err == nil {
			content := string(data)
			if len(content) > MaxBytes {
				content = content[:MaxBytes] + TruncatedNotice
			}
			return content, candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

func Message(content, path string) taskengine.Message {
	return agentservice.AgentsMDMessage(content, path)
}
