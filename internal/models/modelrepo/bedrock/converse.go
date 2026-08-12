package bedrock

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/models/modelrepo"
)

func buildConverseInput(modelName string, messages []modelrepo.Message, cfg *modelrepo.ChatConfig, maxOutputTokens int) (*bedrockruntime.ConverseInput, map[string]string, error) {
	// No audio encoding on this wire format; refuse instead of dropping silently.
	if err := modelrepo.RefuseAudioInput("bedrock", modelName, messages); err != nil {
		return nil, nil, err
	}

	in := &bedrockruntime.ConverseInput{ModelId: aws.String(modelName)}
	toOriginal := map[string]string{}

	// Bedrock rejects toolUse/toolResult blocks unless the request also carries toolConfig.
	hasTools := false
	if cfg != nil {
		for _, t := range cfg.Tools {
			if strings.ToLower(t.Type) == "function" && t.Function != nil {
				hasTools = true
				break
			}
		}
	}

	var system []types.SystemContentBlock
	var msgs []types.Message

	appendBlocks := func(role types.ConversationRole, blocks []types.ContentBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
			return
		}
		msgs = append(msgs, types.Message{Role: role, Content: blocks})
	}

	for _, m := range messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				system = append(system, &types.SystemContentBlockMemberText{Value: m.Content})
			}
		case "tool":
			if !hasTools {
				appendBlocks(types.ConversationRoleUser, []types.ContentBlock{
					&types.ContentBlockMemberText{Value: fmt.Sprintf("[tool result %s]\n%s", m.ToolCallID, m.Content)},
				})
				continue
			}
			appendBlocks(types.ConversationRoleUser, []types.ContentBlock{
				&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
					ToolUseId: aws.String(m.ToolCallID),
					Content:   []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: m.Content}},
				}},
			})
		case "assistant", "model":
			var blocks []types.ContentBlock
			if m.Content != "" {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Content})
			}
			for _, tc := range m.ToolCalls {
				if !hasTools {
					blocks = append(blocks, &types.ContentBlockMemberText{
						Value: fmt.Sprintf("[tool call %s: %s(%s)]", tc.ID, tc.Function.Name, tc.Function.Arguments),
					})
					continue
				}
				safeName := sanitizeToolName(tc.Function.Name)
				toOriginal[safeName] = tc.Function.Name
				blocks = append(blocks, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
					ToolUseId: aws.String(tc.ID),
					Name:      aws.String(safeName),
					Input:     jsonStringToDocument(tc.Function.Arguments),
				}})
			}
			appendBlocks(types.ConversationRoleAssistant, blocks)
		default: // "user"
			var blocks []types.ContentBlock
			if m.Content != "" {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Content})
			}
			// Unrecognised MIME types are skipped rather than sent with an invalid Format.
			for _, img := range m.Images {
				format, ok := imageFormatFromMime(img.MimeType)
				if !ok {
					continue
				}
				blocks = append(blocks, &types.ContentBlockMemberImage{Value: types.ImageBlock{
					Format: format,
					Source: &types.ImageSourceMemberBytes{Value: img.Data},
				}})
			}
			appendBlocks(types.ConversationRoleUser, blocks)
		}
	}

	in.Messages = msgs
	if len(system) > 0 {
		in.System = system
	}

	if cfg != nil {
		ic := &types.InferenceConfiguration{}
		set := false
		if cfg.MaxTokens != nil && *cfg.MaxTokens > 0 {
			effective, _ := modelrepo.ClampMaxOutputTokens(*cfg.MaxTokens, maxOutputTokens)
			const maxInt32 = int64(1<<31 - 1)
			if int64(effective) > maxInt32 {
				effective = int(maxInt32)
			}
			v := int32(effective)
			ic.MaxTokens = &v
			set = true
		}
		if cfg.Temperature != nil {
			v := float32(*cfg.Temperature)
			ic.Temperature = &v
			set = true
		}
		if cfg.TopP != nil {
			v := float32(*cfg.TopP)
			ic.TopP = &v
			set = true
		}
		if set {
			in.InferenceConfig = ic
		}

		var tools []types.Tool
		for _, t := range cfg.Tools {
			if strings.ToLower(t.Type) != "function" || t.Function == nil {
				continue
			}
			safeName := sanitizeToolName(t.Function.Name)
			toOriginal[safeName] = t.Function.Name
			tools = append(tools, &types.ToolMemberToolSpec{Value: types.ToolSpecification{
				Name:        aws.String(safeName),
				Description: aws.String(t.Function.Description),
				InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(t.Function.Parameters)},
			}})
		}
		if len(tools) > 0 {
			in.ToolConfig = &types.ToolConfiguration{Tools: tools}
		}

		if err := applyBedrockThinking(in, modelName, cfg); err != nil {
			return nil, nil, err
		}

		applyBedrockCachePoints(in, modelName, cfg.CacheHints)
	}

	return in, toOriginal, nil
}

func bedrockSupportsPromptCaching(modelName string) bool {
	base := strings.ToLower(bedrockBaseModelID(modelName))
	return strings.Contains(base, "anthropic.claude") || strings.Contains(base, "amazon.nova")
}

func applyBedrockCachePoints(in *bedrockruntime.ConverseInput, modelName string, hints *modelrepo.CacheHints) {
	if in == nil || hints == nil || !bedrockSupportsPromptCaching(modelName) {
		return
	}
	cp := types.CachePointBlock{Type: types.CachePointTypeDefault}
	if hints.StableTools && in.ToolConfig != nil && len(in.ToolConfig.Tools) > 0 {
		in.ToolConfig.Tools = append(in.ToolConfig.Tools, &types.ToolMemberCachePoint{Value: cp})
	}
	if hints.StableSystem && len(in.System) > 0 {
		in.System = append(in.System, &types.SystemContentBlockMemberCachePoint{Value: cp})
	}
}

func applyBedrockThinking(in *bedrockruntime.ConverseInput, modelName string, cfg *modelrepo.ChatConfig) error {
	if cfg == nil || cfg.Think == nil {
		return nil
	}
	level, ok, err := reasoning.NormalizeOptional(*cfg.Think)
	if err != nil || !ok || level == reasoning.Auto {
		return err
	}

	base := strings.ToLower(bedrockBaseModelID(modelName))
	if !strings.Contains(base, "anthropic.") {
		return fmt.Errorf("bedrock model %s does not support a reasoning config (think=%s); drop think or use an Anthropic Claude model on Bedrock", modelName, level)
	}

	if bedrockClaudeUsesAdaptiveThinking(base) {
		if level == reasoning.Off {
			return fmt.Errorf("bedrock model %s always reasons adaptively and cannot disable thinking (think=off); drop the think setting or use a model with configurable thinking", modelName)
		}
		// Adaptive generations reason by default and reject an explicit thinking config.
		return nil
	}

	if level == reasoning.Off {
		in.AdditionalModelRequestFields = document.NewLazyDocument(map[string]any{
			"thinking": map[string]any{"type": "disabled"},
		})
		return nil
	}

	budget := bedrockThinkingBudget(level)
	// budget_tokens must stay below max_tokens; raise an unset/too-small cap.
	if in.InferenceConfig == nil {
		in.InferenceConfig = &types.InferenceConfiguration{}
	}
	if in.InferenceConfig.MaxTokens == nil || *in.InferenceConfig.MaxTokens <= int32(budget) {
		v := int32(budget) + 4096
		in.InferenceConfig.MaxTokens = &v
	}
	in.AdditionalModelRequestFields = document.NewLazyDocument(map[string]any{
		"thinking": map[string]any{"type": "enabled", "budget_tokens": budget},
	})
	return nil
}

func bedrockThinkingBudget(level string) int {
	switch level {
	case reasoning.Medium:
		return 2048
	case reasoning.High:
		return 4096
	case reasoning.XHigh:
		return 8192
	default: // minimal, low
		return 1024
	}
}

func bedrockClaudeUsesAdaptiveThinking(base string) bool {
	return strings.Contains(base, "claude-opus-4-7") ||
		strings.Contains(base, "claude-opus-4-8") ||
		strings.Contains(base, "claude-fable-5") ||
		strings.Contains(base, "claude-sonnet-5") ||
		strings.Contains(base, "mythos")
}

func decodeConverse(out *bedrockruntime.ConverseOutput, toOriginal map[string]string) (modelrepo.ChatResult, error) {
	if out == nil || out.Output == nil {
		return modelrepo.ChatResult{}, fmt.Errorf("bedrock: empty converse output")
	}
	msgOut, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return modelrepo.ChatResult{}, fmt.Errorf("bedrock: unexpected converse output type %T", out.Output)
	}

	var text, thinking strings.Builder
	var toolCalls []modelrepo.ToolCall
	for _, cb := range msgOut.Value.Content {
		switch v := cb.(type) {
		case *types.ContentBlockMemberText:
			text.WriteString(v.Value)
		case *types.ContentBlockMemberReasoningContent:
			// Redacted reasoning content is skipped.
			if rt, ok := v.Value.(*types.ReasoningContentBlockMemberReasoningText); ok {
				thinking.WriteString(aws.ToString(rt.Value.Text))
			}
		case *types.ContentBlockMemberToolUse:
			name := aws.ToString(v.Value.Name)
			if orig, ok := toOriginal[name]; ok {
				name = orig
			}
			toolCalls = append(toolCalls, newToolCall(
				aws.ToString(v.Value.ToolUseId),
				name,
				documentToJSONString(v.Value.Input),
			))
		}
	}

	if text.Len() == 0 && len(toolCalls) == 0 {
		return modelrepo.ChatResult{}, fmt.Errorf("bedrock: no text or tool calls in response")
	}
	return modelrepo.ChatResult{
		Message:      modelrepo.Message{Role: "assistant", Content: text.String(), Thinking: thinking.String()},
		ToolCalls:    toolCalls,
		Usage:        usageFromConverse(out.Usage),
		FinishReason: string(out.StopReason),
	}, nil
}

func usageFromConverse(u *types.TokenUsage) *modelrepo.TokenUsage {
	if u == nil {
		return nil
	}
	read := int(aws.ToInt32(u.CacheReadInputTokens))
	write := int(aws.ToInt32(u.CacheWriteInputTokens))
	prompt := int(aws.ToInt32(u.InputTokens)) + read + write
	completion := int(aws.ToInt32(u.OutputTokens))
	return &modelrepo.TokenUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		CacheReadTokens:  read,
		CacheWriteTokens: write,
	}
}

func newToolCall(id, name, args string) modelrepo.ToolCall {
	tc := modelrepo.ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

func imageFormatFromMime(mime string) (types.ImageFormat, bool) {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return types.ImageFormatPng, true
	case "image/jpeg", "image/jpg":
		return types.ImageFormatJpeg, true
	case "image/gif":
		return types.ImageFormatGif, true
	case "image/webp":
		return types.ImageFormatWebp, true
	default:
		return "", false
	}
}

func sanitizeToolName(in string) string {
	if in == "" {
		return "tool"
	}
	var b strings.Builder
	for _, r := range in {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	s = strings.Trim(s, "_-")
	if len(s) > 64 {
		s = s[:64]
		s = strings.TrimRight(s, "_-")
	}
	if s == "" {
		return "tool"
	}
	return s
}
