package ollama

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type OllamaPromptClient struct {
	ollamaClient    *ollamaHTTPClient
	modelName       string
	backendURL      string
	maxOutputTokens int
	supportsThink   bool
	tracker         libtracker.ActivityTracker
}

func (o *OllamaPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, *modelrepo.TokenUsage, error) {
	reportErr, reportChange, end := o.tracker.Start(ctx, "prompt", "ollama", "model", o.modelName)
	defer end()

	stream := false
	config := &modelrepo.ChatConfig{}
	modelrepo.WithTemperature(float64(temperature)).Apply(config)
	var think *ThinkValue
	if o.supportsThink {
		think = buildOllamaThink(config)
	}
	req := &GenerateRequest{
		Model:     o.modelName,
		Prompt:    prompt,
		System:    systemInstruction,
		Stream:    &stream,
		Options:   buildOllamaOptions(config, o.maxOutputTokens),
		Think:     think,
		KeepAlive: keepAlive(),
	}

	var (
		content       string
		finalResponse GenerateResponse
	)

	err := o.ollamaClient.Generate(ctx, req, func(gr GenerateResponse) error {
		content += gr.Response
		if gr.Done {
			finalResponse = gr
		}
		return nil
	})
	if err != nil {
		reportErr(err)
		return "", nil, fmt.Errorf("ollama generate API call failed for model %s: %w", o.modelName, err)
	}

	if !finalResponse.Done {
		err := fmt.Errorf("no completion received from ollama for model %s", o.modelName)
		reportErr(err)
		return "", nil, err
	}

	switch finalResponse.DoneReason {
	case "error":
		err := fmt.Errorf("ollama generation error for model %s: %s", o.modelName, content)
		reportErr(err)
		return "", nil, err
	case "length":
		err := fmt.Errorf("token limit reached for model %s (partial response: %q)", o.modelName, content)
		reportErr(err)
		return "", nil, err
	case "stop":
		if content == "" {
			err := fmt.Errorf("empty content from model %s despite normal completion", o.modelName)
			reportErr(err)
			return "", nil, err
		}
	default:
		err := fmt.Errorf("unexpected completion reason %q for model %s", finalResponse.DoneReason, o.modelName)
		reportErr(err)
		return "", nil, err
	}

	reportChange("prompt_completed", map[string]any{
		"content_length": len(content),
		"done_reason":    finalResponse.DoneReason,
	})
	// Same accounting as the chat path: prompt_eval_count already includes
	// KV-cache-reused tokens, so the cache fields stay zero.
	usage := &modelrepo.TokenUsage{
		PromptTokens:     finalResponse.Metrics.PromptEvalCount,
		CompletionTokens: finalResponse.Metrics.EvalCount,
		TotalTokens:      finalResponse.Metrics.PromptEvalCount + finalResponse.Metrics.EvalCount,
	}
	return content, usage, nil
}

var _ modelrepo.LLMPromptExecClient = (*OllamaPromptClient)(nil)
