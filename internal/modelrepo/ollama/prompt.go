package ollama

import (
	"context"
	"fmt"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
	"github.com/ollama/ollama/api"
)

type OllamaPromptClient struct {
	ollamaClient *api.Client
	modelName    string
	backendURL   string
	tracker      libtracker.ActivityTracker
}

// Prompt implements LLMPromptExecClient interface
func (o *OllamaPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, error) {
	// Start tracking the operation
	reportErr, reportChange, end := o.tracker.Start(ctx, "prompt", "ollama", "model", o.modelName)
	defer end()

	stream := false
	think := api.ThinkValue{
		Value: false,
	}
	req := &api.GenerateRequest{
		Model:  o.modelName,
		Prompt: prompt,
		System: systemInstruction,
		Stream: &stream,
		Options: map[string]any{
			"temperature": temperature,
		},
		Think: &think,
	}

	var (
		content       string
		finalResponse api.GenerateResponse
	)

	err := o.ollamaClient.Generate(ctx, req, func(gr api.GenerateResponse) error {
		content += gr.Response
		if gr.Done {
			finalResponse = gr
		}
		return nil
	})
	if err != nil {
		reportErr(err)
		return "", fmt.Errorf("ollama generate API call failed for model %s: %w", o.modelName, err)
	}

	if !finalResponse.Done {
		err := fmt.Errorf("no completion received from ollama for model %s", o.modelName)
		reportErr(err)
		return "", err
	}

	switch finalResponse.DoneReason {
	case "error":
		err := fmt.Errorf("ollama generation error for model %s: %s", o.modelName, content)
		reportErr(err)
		return "", err
	case "length":
		err := fmt.Errorf("token limit reached for model %s (partial response: %q)", o.modelName, content)
		reportErr(err)
		return "", err
	case "stop":
		if content == "" {
			err := fmt.Errorf("empty content from model %s despite normal completion", o.modelName)
			reportErr(err)
			return "", err
		}
	default:
		err := fmt.Errorf("unexpected completion reason %q for model %s", finalResponse.DoneReason, o.modelName)
		reportErr(err)
		return "", err
	}

	reportChange("prompt_completed", map[string]any{
		"content_length": len(content),
		"done_reason":    finalResponse.DoneReason,
	})
	return content, nil
}

var _ modelrepo.LLMPromptExecClient = (*OllamaPromptClient)(nil)
