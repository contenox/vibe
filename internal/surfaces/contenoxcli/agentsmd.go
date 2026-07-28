package contenoxcli

import (
	"os"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentsmd"
)

const (
	agentsMDFilename  = agentsmd.Filename
	maxAgentsMDBytes  = agentsmd.MaxBytes
	agentsMDTruncated = agentsmd.TruncatedNotice
)

func LoadAgentsMD(startDir string) (string, string, bool) {
	return agentsmd.Load(startDir)
}

func AgentsMDMessage(content, path string) taskengine.Message {
	return agentsmd.Message(content, path)
}

func loadAgentsMDFromCwd() (string, string) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	content, path, ok := LoadAgentsMD(cwd)
	if !ok {
		return "", ""
	}
	return content, path
}
