// init.go implements the vibe init subcommand (scaffold .contenox/).
package vibecli

import (
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

//go:embed config.yaml
var initConfig string

//go:embed chain-vibes.json
var initChain string

// RunInit scaffolds .contenox/ (config and default chain). If force is true, overwrites existing files.
func RunInit(force bool) {
	cwd, err := os.Getwd()
	if err != nil {
		slog.Error("Cannot get current directory", "error", err)
		os.Exit(1)
	}
	contenoxDir := filepath.Join(cwd, ".contenox")
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		slog.Error("Failed to create .contenox directory", "error", err)
		os.Exit(1)
	}
	configPath := filepath.Join(contenoxDir, "config.yaml")
	chainPath := filepath.Join(contenoxDir, "default-chain.json")
	writeFile := func(path, content string) bool {
		if !force {
			if _, err := os.Stat(path); err == nil {
				fmt.Printf("  %s already exists (use --force to overwrite)\n", path)
				return false
			}
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			slog.Error("Failed to write file", "path", path, "error", err)
			os.Exit(1)
		}
		fmt.Printf("  Created %s\n", path)
		return true
	}
	writeFile(configPath, initConfig)
	writeFile(chainPath, initChain)
	fmt.Println("Done. The default chain is vibes: natural language → shell commands (e.g. list files, run commands).")
	fmt.Println("Next: start Ollama (ollama serve), pull a tool-capable model (e.g. ollama pull qwen2.5:7b), then run:")
	fmt.Println("  vibe list files in my home directory")
	fmt.Println("  vibe what is in /tmp")
	fmt.Println("To use OpenAI, vLLM, or Gemini, add backends and set default_provider/default_model in .contenox/config.yaml.")
}
