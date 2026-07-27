package bedrock

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/contenox/beam/internal/kernel/reasoning"
	"github.com/contenox/beam/internal/models/modelrepo"
)

// buildConverseInput maps neutral messages + config into a Converse request.
// Adjacent same-role messages are merged (Bedrock requires alternating roles).
// Bedrock tool names must match ^[a-zA-Z0-9_-]{1,64}$, so tool names are
// sanitized before being sent; the returned map lets the caller translate
// sanitized names in the response back to the caller's original names.
// A reasoning request (cfg.Think) that the model cannot honor is a hard error,
// never a silent drop.
func buildConverseInput(modelName string, messages []modelrepo.Message, cfg *modelrepo.ChatConfig, maxOutputTokens int) (*bedrockruntime.ConverseInput, map[string]string, error) {
	in := &bedrockruntime.ConverseInput{ModelId: aws.String(modelName)}
	toOriginal := map[string]string{}

	// Bedrock rejects toolUse/toolResult content blocks unless the request also
	// carries toolConfig. Tasks without tools (recovery/summarise steps) still
	// receive histories from tool-using turns, so those blocks are rendered as
	// plain text instead — a synthetic toolConfig would invite the model to
	// call tools this task cannot execute.
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
			// Vision: append an image content block per attachment, after any
			// text block. Bedrock takes the raw image bytes in the Bytes source
			// (the SDK base64-encodes them on the wire). Images with an
			// unrecognised MIME type are skipped rather than sent with an
			// invalid Format that Bedrock would reject.
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

// bedrockSupportsPromptCaching gates cachePoint insertion by model family.
// AWS documents prompt caching for Anthropic Claude (3.5 Sonnet v2 / 3.7
// onward) and Amazon Nova on the Converse API; the gate is deliberately
// family-coarse and unsupported models simply get no cachePoints (Bedrock
// rejects cachePoint blocks on unsupported models rather than ignoring them,
// so silence here means omission, not a server-side no-op).
func bedrockSupportsPromptCaching(modelName string) bool {
	base := strings.ToLower(bedrockBaseModelID(modelName))
	return strings.Contains(base, "anthropic.claude") || strings.Contains(base, "amazon.nova")
}

// applyBedrockCachePoints inserts Converse cachePoint blocks where the hints
// assert stability: after the last tool definition (tools render first) and
// after the last system block. Placement is metadata-only — cachePoint is its
// own union member and never alters model-visible content. History
// breakpoints are not placed on Bedrock (Claude gets simplified cache
// management server-side; blueprint §4.3). The advisory hint TTL is ignored:
// the 1h tier is limited to 4.5-class Claude models with ordering
// constraints, so the 5m sliding default is used until a model-accurate gate
// exists. At most 2 of the 4 allowed checkpoints are used.
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

// applyBedrockThinking maps cfg.Think onto Bedrock's documented reasoning
// config for Claude models: additionalModelRequestFields carrying
// {"thinking": {"type": "enabled", "budget_tokens": N}} (the Anthropic-native
// passthrough AWS documents for Claude on Converse). Models that cannot honor
// the request refuse loudly:
//   - non-Claude models have no reasoning config on Converse;
//   - adaptive-thinking Claude generations reject an explicit enabled/disabled
//     config (they always reason), so only "off" is refused for them.
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
		// Adaptive generations reason by default and reject an explicit
		// thinking config; the request is already satisfied.
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

// bedrockThinkingBudget mirrors the direct-Anthropic budget ladder
// (anthropic/client.go); Bedrock documents a 1024-token minimum.
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

// bedrockClaudeUsesAdaptiveThinking mirrors the direct-provider model split
// (anthropic/client.go anthropicRequiresAdaptiveThinking): these generations
// reject thinking.type enabled/disabled with a 400 and reason adaptively.
func bedrockClaudeUsesAdaptiveThinking(base string) bool {
	return strings.Contains(base, "claude-opus-4-7") ||
		strings.Contains(base, "claude-opus-4-8") ||
		strings.Contains(base, "claude-fable-5") ||
		strings.Contains(base, "claude-sonnet-5") ||
		strings.Contains(base, "mythos")
}

// decodeConverse maps a Converse response into a neutral ChatResult. toOriginal
// translates sanitized tool names (as sent to Bedrock) back to the caller's
// original tool names; unknown names are passed through unchanged.
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
			// reasoningContent is a union; the readable form is
			// reasoningText{text, signature}. Redacted content is skipped.
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
		Message:   modelrepo.Message{Role: "assistant", Content: text.String(), Thinking: thinking.String()},
		ToolCalls: toolCalls,
		Usage:     usageFromConverse(out.Usage),
	}, nil
}

// usageFromConverse maps the Converse usage report onto the neutral
// TokenUsage. Bedrock's inputTokens counts ONLY the uncached remainder;
// cacheRead/cacheWriteInputTokens are separate, and per the normalization
// rule PromptTokens is the sum of all three. TotalTokens is recomputed from
// the normalized prompt count rather than trusted from the wire.
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

// imageFormatFromMime maps an image MIME type to the Bedrock Converse
// ImageFormat enum. Only the formats Bedrock accepts (png, jpeg, gif, webp)
// are recognised; any other type returns ok=false so the caller can skip the
// attachment instead of sending a Format value Bedrock would reject.
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

// sanitizeToolName replaces invalid characters with '_' and trims leading/trailing separators.
// Allowed: letters, digits, underscore, hyphen. Maximum length is 64 characters.
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
	// avoid leading/trailing separators
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
