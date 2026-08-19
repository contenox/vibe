package scriptedtest

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type Provider struct {
	modelName  string
	scriptPath string
	caps       modelrepo.CapabilityConfig
	tracker    libtracker.ActivityTracker
	dimensions int
}

func NewProvider(modelName, scriptPath string, caps modelrepo.CapabilityConfig, embedDimensions int, tracker libtracker.ActivityTracker) *Provider {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	if embedDimensions <= 0 {
		embedDimensions = defaultEmbedDimensions
	}
	return &Provider{
		modelName:  modelName,
		scriptPath: scriptPath,
		caps:       caps,
		tracker:    tracker,
		dimensions: embedDimensions,
	}
}

func (p *Provider) GetBackendIDs() []string { return []string{p.scriptPath} }

func (p *Provider) ModelName() string { return p.modelName }

func (p *Provider) GetID() string { return modelrepo.ScriptedTestBackendType + ":" + p.modelName }

func (p *Provider) GetType() string { return modelrepo.ScriptedTestBackendType }

func (p *Provider) GetContextLength() int { return p.caps.ContextLength }

func (p *Provider) GetMaxOutputTokens() int { return p.caps.MaxOutputTokens }

func (p *Provider) CanChat() bool { return p.caps.CanChat }

func (p *Provider) CanEmbed() bool { return p.caps.CanEmbed }

func (p *Provider) CanStream() bool { return p.caps.CanStream }

func (p *Provider) CanPrompt() bool { return p.caps.CanPrompt }

func (p *Provider) CanThink() bool { return p.caps.CanThink }

func (p *Provider) CanVision() bool { return p.caps.CanVision }

func (p *Provider) CanAudio() bool { return p.caps.CanAudio }

// scriptFor prefers the backend id the resolver picked, which is the registered backend's script path.
func (p *Provider) scriptFor(backendID string) (*Script, error) {
	path := strings.TrimSpace(backendID)
	if path == "" {
		path = p.scriptPath
	}
	return Load(path)
}

func (p *Provider) GetChatConnection(ctx context.Context, backendID string) (modelrepo.LLMChatClient, error) {
	if !p.CanChat() {
		return nil, fmt.Errorf("provider %s (model %s) does not support chat", p.GetID(), p.modelName)
	}
	script, err := p.scriptFor(backendID)
	if err != nil {
		return nil, err
	}
	return &chatClient{script: script, modelName: p.modelName, tracker: p.tracker}, nil
}

func (p *Provider) GetStreamConnection(ctx context.Context, backendID string) (modelrepo.LLMStreamClient, error) {
	if !p.CanStream() {
		return nil, fmt.Errorf("provider %s (model %s) does not support streaming", p.GetID(), p.modelName)
	}
	script, err := p.scriptFor(backendID)
	if err != nil {
		return nil, err
	}
	return &streamClient{script: script, modelName: p.modelName, tracker: p.tracker}, nil
}

func (p *Provider) GetPromptConnection(ctx context.Context, backendID string) (modelrepo.LLMPromptExecClient, error) {
	if !p.CanPrompt() {
		return nil, fmt.Errorf("provider %s (model %s) does not support prompt execution", p.GetID(), p.modelName)
	}
	script, err := p.scriptFor(backendID)
	if err != nil {
		return nil, err
	}
	return &promptClient{script: script, modelName: p.modelName, tracker: p.tracker}, nil
}

func (p *Provider) GetEmbedConnection(ctx context.Context, backendID string) (modelrepo.LLMEmbedClient, error) {
	if !p.CanEmbed() {
		return nil, fmt.Errorf("provider %s (model %s) does not support embeddings", p.GetID(), p.modelName)
	}
	return &embedClient{dimensions: p.dimensions}, nil
}

type chatClient struct {
	script    *Script
	modelName string
	tracker   libtracker.ActivityTracker
}

func (c *chatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	reportErr, _, end := c.tracker.Start(ctx, "chat", modelrepo.ScriptedTestBackendType, "model", c.modelName, "script", c.script.Path)
	defer end()

	turn, index, err := c.script.Next("chat")
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	calls := toolCallsFor(turn, index)
	result := modelrepo.ChatResult{
		Message: modelrepo.Message{
			Role:      "assistant",
			Content:   turn.Text,
			Thinking:  turn.Thinking,
			ToolCalls: calls,
		},
		ToolCalls:    calls,
		FinishReason: finishReason(turn, calls),
	}
	if turn.Usage != nil {
		result.Usage = &modelrepo.TokenUsage{
			PromptTokens:     turn.Usage.PromptTokens,
			CompletionTokens: turn.Usage.CompletionTokens,
			TotalTokens:      turn.Usage.TotalTokens,
		}
	}
	return result, nil
}

type streamClient struct {
	script    *Script
	modelName string
	tracker   libtracker.ActivityTracker
}

// Stream consumes its turn before the channel is handed back, so turn order follows call order rather than consumption order.
func (c *streamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	reportErr, _, end := c.tracker.Start(ctx, "stream", modelrepo.ScriptedTestBackendType, "model", c.modelName, "script", c.script.Path)
	defer end()

	turn, index, err := c.script.Next("stream")
	if err != nil {
		reportErr(err)
		return nil, err
	}
	calls := toolCallsFor(turn, index)

	ch := make(chan *modelrepo.StreamParcel)
	go func() {
		defer close(ch)
		send := func(p *modelrepo.StreamParcel) bool {
			select {
			case ch <- p:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, chunk := range chunks(turn.Thinking) {
			if !send(&modelrepo.StreamParcel{Thinking: chunk}) {
				return
			}
		}
		for _, chunk := range chunks(turn.Text) {
			if !send(&modelrepo.StreamParcel{Data: chunk}) {
				return
			}
		}
		for i, call := range calls {
			delta := &modelrepo.ToolCallDelta{
				Index:        i,
				ID:           call.ID,
				Type:         call.Type,
				Name:         call.Function.Name,
				ArgsFragment: call.Function.Arguments,
			}
			if !send(&modelrepo.StreamParcel{ToolCall: delta}) {
				return
			}
		}
		terminal := &modelrepo.StreamTerminal{FinishReason: finishReason(turn, calls)}
		if turn.Usage != nil {
			terminal.Usage = &modelrepo.TokenUsage{
				PromptTokens:     turn.Usage.PromptTokens,
				CompletionTokens: turn.Usage.CompletionTokens,
				TotalTokens:      turn.Usage.TotalTokens,
			}
		}
		send(&modelrepo.StreamParcel{Terminal: terminal})
	}()
	return ch, nil
}

type promptClient struct {
	script    *Script
	modelName string
	tracker   libtracker.ActivityTracker
}

func (c *promptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, *modelrepo.TokenUsage, error) {
	reportErr, _, end := c.tracker.Start(ctx, "prompt", modelrepo.ScriptedTestBackendType, "model", c.modelName, "script", c.script.Path)
	defer end()

	turn, index, err := c.script.Next("prompt")
	if err != nil {
		reportErr(err)
		return "", nil, err
	}
	if turn.Text == "" {
		err := fmt.Errorf("scripted-test script %q turn %d has no \"text\": a prompt call cannot replay a tool-call turn", c.script.Path, index)
		reportErr(err)
		return "", nil, err
	}
	var usage *modelrepo.TokenUsage
	if turn.Usage != nil {
		usage = &modelrepo.TokenUsage{
			PromptTokens:     turn.Usage.PromptTokens,
			CompletionTokens: turn.Usage.CompletionTokens,
			TotalTokens:      turn.Usage.TotalTokens,
		}
	}
	return turn.Text, usage, nil
}

type embedClient struct {
	dimensions int
}

// Embed never consumes a turn: an embedding is not a model turn, and spending one would desync the dialog.
func (c *embedClient) Embed(ctx context.Context, prompt string) ([]float64, error) {
	vector := make([]float64, c.dimensions)
	for i := range vector {
		h := fnv.New64a()
		fmt.Fprintf(h, "%d\x00%s", i, prompt)
		vector[i] = float64(h.Sum64()%2000)/1000 - 1
	}
	return vector, nil
}

func finishReason(turn Turn, calls []modelrepo.ToolCall) string {
	if turn.FinishReason != "" {
		return turn.FinishReason
	}
	if len(calls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func toolCallsFor(turn Turn, turnIndex int) []modelrepo.ToolCall {
	if len(turn.ToolCalls) == 0 {
		return nil
	}
	calls := make([]modelrepo.ToolCall, 0, len(turn.ToolCalls))
	for i, scripted := range turn.ToolCalls {
		arguments, err := callArguments(scripted)
		if err != nil {
			arguments = "{}"
		}
		call := modelrepo.ToolCall{
			ID:   scripted.ID,
			Type: "function",
		}
		if call.ID == "" {
			call.ID = fmt.Sprintf("scripted-test-%d-%d", turnIndex, i)
		}
		call.Function.Name = scripted.Name
		call.Function.Arguments = arguments
		calls = append(calls, call)
	}
	return calls
}

func chunks(text string) []string {
	if text == "" {
		return nil
	}
	return strings.SplitAfter(text, " ")
}

var _ modelrepo.Provider = (*Provider)(nil)
