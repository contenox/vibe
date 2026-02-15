// init.go implements the vibe init subcommand (scaffold .contenox/).
package vibecli

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

const initConfigYAML = `# Default config created by 'vibe init'. Edit as needed.
default_chain: default-chain.json
backends:
  - name: default
    type: ollama
    base_url: http://127.0.0.1:11434
default_provider: ollama
default_model: phi3:3.8b
`

const initDefaultChainJSON = `{"id":"qa-ollama","debug":true,"description":"Single prompt with Ollama","tasks":[{"id":"generate_response","description":"Generate answer","handler":"prompt_to_string","system_instruction":"You are a helpful assistant. Reply concisely.","prompt_template":"{{.input}}","input_var":"input","transition":{"branches":[{"operator":"default","goto":"end"}]}}],"token_limit":2048}
`

func runInit(args []string) {
	force := false
	for i := 0; i < len(args); i++ {
		if args[i] == "-force" || args[i] == "--force" {
			force = true
			break
		}
	}
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
				fmt.Printf("  %s already exists (use -force to overwrite)\n", path)
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
	writeFile(configPath, initConfigYAML)
	writeFile(chainPath, initDefaultChainJSON)
	fmt.Println("Done. Next: start Ollama (ollama serve), pull a model (e.g. ollama pull phi3:3.8b), then run: vibe -input 'Hello'")
	fmt.Println("To use OpenAI, vLLM, or Gemini, add backends and set default_provider/default_model in .contenox/config.yaml.")
}
