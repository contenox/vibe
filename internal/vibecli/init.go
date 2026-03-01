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
	fmt.Println("Done.")
	fmt.Println("")
	fmt.Println("Next steps:")
	fmt.Println("")
	fmt.Println("  1. Pick your LLM provider — open .contenox/config.yaml and follow the comments:")
	fmt.Println("       Local (Ollama): ollama serve && ollama pull qwen2.5:7b")
	fmt.Println("       OpenAI:         export OPENAI_API_KEY=sk-... (then uncomment the OpenAI section)")
	fmt.Println("       Gemini:         export GEMINI_API_KEY=...    (then uncomment the Gemini section)")
	fmt.Println("")
	fmt.Println("  2. Try a one-shot command:")
	fmt.Println("       vibe list files in my home directory")
	fmt.Println("       vibe what is in /tmp")
	fmt.Println("")
	fmt.Println("  3. Plan and execute a multi-step task:")
	fmt.Println("       vibe plan new \"create a TODOS.md from all TODO comments in the codebase\"")
	fmt.Println("       vibe plan next --auto")
	fmt.Println("")
	fmt.Println("  Run 'vibe --help' for full usage.")
}
