// Package messages is a transport-agnostic codec for Anthropic's Messages API
// wire format. It maps between neutral modelrepo types and Anthropic's JSON
// shape and does no I/O.
package messages

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/models/modelrepo"
)

// DefaultMaxTokens is used when the caller does not set ChatConfig.MaxTokens.
const DefaultMaxTokens = 4096

// Request is the Anthropic Messages request body. System is a plain string, or
// a []wireBlock of text blocks when a cache breakpoint is placed on it.
type Request struct {
	Model        string          `json:"model,omitempty"`
	MaxTokens    int             `json:"max_tokens"`
	System       any             `json:"system,omitempty"`
	Messages     []wireMessage   `json:"messages"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	Tools        []wireTool      `json:"tools,omitempty"`
	Thinking     *ThinkingConfig `json:"thinking,omitempty"`
	OutputConfig *OutputConfig   `json:"output_config,omitempty"`
	Stream       bool            `json:"stream,omitempty"`
}

// CacheControl marks a cache breakpoint on a content block or tool definition:
// everything rendered up to and including the marked element is cached.
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// MaxCacheBreakpoints is Anthropic's per-request limit on cache_control breakpoints.
const MaxCacheBreakpoints = 4

// ThinkingBlocksMetaKey is the ToolCall.ProviderMeta key under which the
// assistant turn's thinking blocks round-trip, as the JSON array of wire blocks.
const ThinkingBlocksMetaKey = "anthropic_thinking_blocks"

type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type OutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// image
	Source *wireImageSource `json:"source,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// redacted_thinking
	Data         string        `json:"data,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type wireImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type wireTool struct {
	Name         string        `json:"name"`
	Description  string        `json:"description,omitempty"`
	InputSchema  any           `json:"input_schema,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// Build converts neutral messages and config into an Anthropic Messages Request.
// The transport must still set Model.
func Build(messages []modelrepo.Message, cfg *modelrepo.ChatConfig) (Request, map[string]string) {
	req := Request{MaxTokens: DefaultMaxTokens}
	nameMap := make(map[string]string)
	origToSanitized := make(map[string]string)

	if cfg != nil {
		if cfg.MaxTokens != nil && *cfg.MaxTokens > 0 {
			req.MaxTokens = *cfg.MaxTokens
		}
		req.Temperature = cfg.Temperature
		req.TopP = cfg.TopP

		seen := map[string]int{}
		for _, t := range cfg.Tools {
			if strings.ToLower(t.Type) != "function" || t.Function == nil {
				continue
			}
			orig := t.Function.Name
			name := sanitizeToolName(orig)
			if name == "" {
				name = "tool"
			}
			name = uniquifyToolName(seen, name)
			nameMap[name] = orig
			origToSanitized[orig] = name

			req.Tools = append(req.Tools, wireTool{
				Name:        name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}

	var systemParts []string
	// srcIdx[j] is the index into the neutral messages slice that produced wire message j.
	var srcIdx []int
	for i, m := range messages {
		before := len(req.Messages)
		switch m.Role {
		case "system":
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case "tool":
			req.Messages = append(req.Messages, wireMessage{
				Role: "user",
				Content: []wireBlock{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   m.Content,
				}},
			})
		case "assistant", "model":
			var blocks []wireBlock
			// Anthropic requires signed thinking blocks to precede tool_use blocks.
			blocks = append(blocks, thinkingBlocksFromToolCalls(m.ToolCalls)...)
			if m.Content != "" {
				blocks = append(blocks, wireBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				name := tc.Function.Name
				if san, ok := origToSanitized[name]; ok {
					name = san
				} else {
					name = sanitizeToolName(name)
					if name == "" {
						name = "tool"
					}
				}

				input := json.RawMessage(tc.Function.Arguments)
				if len(strings.TrimSpace(tc.Function.Arguments)) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, wireBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  name,
					Input: input,
				})
			}
			if len(blocks) > 0 {
				req.Messages = append(req.Messages, wireMessage{Role: "assistant", Content: blocks})
			}
		default:
			if len(m.Images) > 0 {
				req.Messages = append(req.Messages, wireMessage{
					Role:    "user",
					Content: imageContentBlocks(m),
				})
			} else if m.Content != "" {
				req.Messages = append(req.Messages, wireMessage{
					Role:    "user",
					Content: []wireBlock{{Type: "text", Text: m.Content}},
				})
			}
		}
		for range req.Messages[before:] {
			srcIdx = append(srcIdx, i)
		}
	}
	if len(systemParts) > 0 {
		req.System = strings.Join(systemParts, "\n\n")
	}
	if cfg != nil {
		applyCacheHints(&req, cfg.CacheHints, srcIdx)
	}
	return req, nameMap
}

func applyCacheHints(req *Request, hints *modelrepo.CacheHints, srcIdx []int) {
	if hints == nil {
		return
	}
	cc := func() *CacheControl {
		c := &CacheControl{Type: "ephemeral"}
		if hints.TTL >= time.Hour {
			c.TTL = "1h"
		}
		return c
	}
	placed := 0
	if hints.StableTools && len(req.Tools) > 0 && placed < MaxCacheBreakpoints {
		req.Tools[len(req.Tools)-1].CacheControl = cc()
		placed++
	}
	if hints.StableSystem && placed < MaxCacheBreakpoints {
		if sys, ok := req.System.(string); ok && sys != "" {
			req.System = []wireBlock{{Type: "text", Text: sys, CacheControl: cc()}}
			placed++
		}
	}
	if hints.StableHistoryLen > 0 && placed < MaxCacheBreakpoints {
		last := -1
		for j, src := range srcIdx {
			if src < hints.StableHistoryLen {
				last = j
			}
		}
		if last >= 0 && last < len(req.Messages) {
			if blocks := req.Messages[last].Content; len(blocks) > 0 {
				if t := blocks[len(blocks)-1].Type; t != "thinking" && t != "redacted_thinking" {
					blocks[len(blocks)-1].CacheControl = cc()
				}
			}
		}
	}
}

func thinkingBlocksFromToolCalls(toolCalls []modelrepo.ToolCall) []wireBlock {
	for _, tc := range toolCalls {
		raw := tc.ProviderMeta[ThinkingBlocksMetaKey]
		if raw == "" {
			continue
		}
		var blocks []wireBlock
		if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
			return nil
		}
		return blocks
	}
	return nil
}

// StripThinkingBlocks removes replayed thinking blocks from every message, and
// any message left empty. Transports call it when the request does not enable thinking.
func StripThinkingBlocks(req *Request) {
	if req == nil {
		return
	}
	msgs := req.Messages[:0]
	for _, m := range req.Messages {
		blocks := m.Content[:0]
		for _, b := range m.Content {
			if b.Type == "thinking" || b.Type == "redacted_thinking" {
				continue
			}
			blocks = append(blocks, b)
		}
		m.Content = blocks
		if len(m.Content) == 0 {
			continue
		}
		msgs = append(msgs, m)
	}
	req.Messages = msgs
}

func imageContentBlocks(m modelrepo.Message) []wireBlock {
	blocks := make([]wireBlock, 0, len(m.Images)+1)
	if m.Content != "" {
		blocks = append(blocks, wireBlock{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		blocks = append(blocks, wireBlock{
			Type: "image",
			Source: &wireImageSource{
				Type:      "base64",
				MediaType: img.MimeType,
				Data:      base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	return blocks
}

// Response is the non-streaming Anthropic Messages response body.
type Response struct {
	Role       string          `json:"role"`
	Content    []responseBlock `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      *wireUsage      `json:"usage"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func normalizeUsage(u wireUsage) modelrepo.TokenUsage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return modelrepo.TokenUsage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

type responseBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
}

// DecodeResponse parses a non-streaming response into a neutral ChatResult.
func DecodeResponse(raw []byte, nameMap map[string]string) (modelrepo.ChatResult, error) {
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return modelrepo.ChatResult{}, fmt.Errorf("messages: decode response: %w", err)
	}
	if resp.StopReason == "refusal" {
		return modelrepo.ChatResult{}, modelrepo.ErrRefused
	}
	if len(resp.Content) == 0 {
		return modelrepo.ChatResult{}, fmt.Errorf("messages: empty content (stop_reason=%s)", resp.StopReason)
	}
	var text, thinking strings.Builder
	var toolCalls []modelrepo.ToolCall
	var thinkingBlocks []wireBlock
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			thinking.WriteString(b.Thinking)
			thinkingBlocks = append(thinkingBlocks, wireBlock{Type: "thinking", Thinking: b.Thinking, Signature: b.Signature})
		case "redacted_thinking":
			thinkingBlocks = append(thinkingBlocks, wireBlock{Type: "redacted_thinking", Data: b.Data})
		case "tool_use":
			args := string(b.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			name := b.Name
			if orig, ok := nameMap[name]; ok && orig != "" {
				name = orig
			}
			toolCalls = append(toolCalls, newToolCall(b.ID, name, args))
		}
	}
	attachThinkingBlocks(toolCalls, thinkingBlocks)
	role := resp.Role
	if role == "" {
		role = "assistant"
	}
	var usage *modelrepo.TokenUsage
	if resp.Usage != nil {
		u := normalizeUsage(*resp.Usage)
		usage = &u
	}
	return modelrepo.ChatResult{
		Message: modelrepo.Message{
			Role:     role,
			Content:  text.String(),
			Thinking: thinking.String(),
		},
		ToolCalls:    toolCalls,
		Usage:        usage,
		FinishReason: resp.StopReason,
	}, nil
}

func attachThinkingBlocks(toolCalls []modelrepo.ToolCall, blocks []wireBlock) {
	if len(toolCalls) == 0 || len(blocks) == 0 {
		return
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return
	}
	if toolCalls[0].ProviderMeta == nil {
		toolCalls[0].ProviderMeta = map[string]string{}
	}
	toolCalls[0].ProviderMeta[ThinkingBlocksMetaKey] = string(raw)
}

func newToolCall(id, name, args string) modelrepo.ToolCall {
	tc := modelrepo.ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

type streamEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Data string `json:"data"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Message struct {
		Usage streamUsage `json:"usage"`
	} `json:"message"`
	Usage streamUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type streamUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// StreamDecoder translates streamed Messages SSE events into raw-delta parcels.
// It does not assemble tool calls; that is left to modelrepo.StreamAssembler.
type StreamDecoder struct {
	nameMap    map[string]string
	stopReason string
	usage      wireUsage
	sawUsage   bool

	openThinking     *wireBlock
	thinkingBlocks   []wireBlock
	thinkingAttached bool
}

func NewStreamDecoder(nameMap map[string]string) *StreamDecoder {
	return &StreamDecoder{nameMap: nameMap}
}

// DecodeLine parses one SSE data payload and returns the parcels it carries, in
// wire order. An Anthropic error event returns an error.
func (d *StreamDecoder) DecodeLine(payload []byte) ([]*modelrepo.StreamParcel, error) {
	var ev streamEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, fmt.Errorf("messages: decode stream event: %w", err)
	}
	switch ev.Type {
	case "error":
		return nil, fmt.Errorf("messages: in-stream error event: %s: %s", ev.Error.Type, ev.Error.Message)
	case "message_start":
		d.recordUsage(ev.Message.Usage)
		return nil, nil
	case "message_delta":
		if ev.Delta.StopReason != "" {
			d.stopReason = ev.Delta.StopReason
		}
		d.recordUsage(ev.Usage)
		return nil, nil
	case "content_block_start":
		switch ev.ContentBlock.Type {
		case "tool_use":
			name := ev.ContentBlock.Name
			if orig, ok := d.nameMap[name]; ok && orig != "" {
				name = orig
			}
			delta := &modelrepo.ToolCallDelta{
				Index: ev.Index,
				ID:    ev.ContentBlock.ID,
				Type:  "function",
				Name:  name,
			}
			if meta := d.takeThinkingBlocksMeta(); meta != "" {
				delta.ProviderMeta = map[string]string{ThinkingBlocksMetaKey: meta}
			}
			return []*modelrepo.StreamParcel{{ToolCall: delta}}, nil
		case "thinking":
			d.openThinking = &wireBlock{Type: "thinking"}
		case "redacted_thinking":
			d.thinkingBlocks = append(d.thinkingBlocks, wireBlock{Type: "redacted_thinking", Data: ev.ContentBlock.Data})
		}
		return nil, nil
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				return []*modelrepo.StreamParcel{{Data: ev.Delta.Text}}, nil
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				if d.openThinking != nil {
					d.openThinking.Thinking += ev.Delta.Thinking
				}
				return []*modelrepo.StreamParcel{{Thinking: ev.Delta.Thinking}}, nil
			}
		case "signature_delta":
			if d.openThinking != nil {
				d.openThinking.Signature += ev.Delta.Signature
			}
		case "input_json_delta":
			if ev.Delta.PartialJSON != "" {
				return []*modelrepo.StreamParcel{{ToolCall: &modelrepo.ToolCallDelta{
					Index:        ev.Index,
					ArgsFragment: ev.Delta.PartialJSON,
				}}}, nil
			}
		}
		return nil, nil
	case "content_block_stop":
		if d.openThinking != nil {
			d.thinkingBlocks = append(d.thinkingBlocks, *d.openThinking)
			d.openThinking = nil
		}
		return nil, nil
	default:
		// message_stop, ping
		return nil, nil
	}
}

func (d *StreamDecoder) takeThinkingBlocksMeta() string {
	if d.thinkingAttached || len(d.thinkingBlocks) == 0 {
		return ""
	}
	raw, err := json.Marshal(d.thinkingBlocks)
	if err != nil {
		return ""
	}
	d.thinkingAttached = true
	return string(raw)
}

func (d *StreamDecoder) recordUsage(u streamUsage) {
	if u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0 {
		return
	}
	d.sawUsage = true
	if u.InputTokens != 0 {
		d.usage.InputTokens = u.InputTokens
	}
	if u.OutputTokens != 0 {
		d.usage.OutputTokens = u.OutputTokens
	}
	if u.CacheCreationInputTokens != 0 {
		d.usage.CacheCreationInputTokens = u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens != 0 {
		d.usage.CacheReadInputTokens = u.CacheReadInputTokens
	}
}

// Finish returns the terminal parcel: the stop reason plus accumulated usage.
// Callers emit it after the SSE stream ends cleanly.
func (d *StreamDecoder) Finish() *modelrepo.StreamParcel {
	term := &modelrepo.StreamTerminal{FinishReason: d.stopReason}
	if d.sawUsage {
		u := normalizeUsage(d.usage)
		term.Usage = &u
	}
	return &modelrepo.StreamParcel{Terminal: term}
}

func sanitizeToolName(in string) string {
	if in == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range in {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_-")
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

func uniquifyToolName(seen map[string]int, name string) string {
	if _, ok := seen[name]; !ok {
		seen[name] = 1
		return name
	}
	i := seen[name]
	for {
		suffix := fmt.Sprintf("_%d", i)
		base := name
		if len(base)+len(suffix) > 128 {
			base = base[:128-len(suffix)]
		}
		candidate := base + suffix
		if _, ok := seen[candidate]; !ok {
			seen[name] = i + 1
			seen[candidate] = 1
			return candidate
		}
		i++
	}
}
