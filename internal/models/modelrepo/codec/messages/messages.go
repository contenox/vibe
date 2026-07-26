// Package messages is a transport-agnostic codec for Anthropic's Messages API
// wire format (request, content-block response, and named-SSE-event streaming).
// It maps between contenox's neutral modelrepo types and Anthropic's JSON shape.
//
// It does NO I/O. The transport (api.anthropic.com) supplies the envelope:
// model in the body, version via the `anthropic-version` header, auth via
// `x-api-key`. This lets the direct Anthropic provider stay a thin transport
// wrapper around the shared codec.
package messages

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/contenox/beam/internal/models/modelrepo"
)

// DefaultMaxTokens is used when the caller does not set ChatConfig.MaxTokens.
// Anthropic requires max_tokens; it has no "unlimited" sentinel.
const DefaultMaxTokens = 4096

// Request is the Anthropic Messages request body.
type Request struct {
	// Model is omitted for Vertex (model lives in the URL) and set for direct.
	Model string `json:"model,omitempty"`
	// AnthropicVersion is set by the Vertex transport ("vertex-2023-10-16");
	// empty for direct (sent as a header instead).
	AnthropicVersion string          `json:"anthropic_version,omitempty"`
	MaxTokens        int             `json:"max_tokens"`
	System           string          `json:"system,omitempty"`
	Messages         []wireMessage   `json:"messages"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	Tools            []wireTool      `json:"tools,omitempty"`
	Thinking         *ThinkingConfig `json:"thinking,omitempty"`
	OutputConfig     *OutputConfig   `json:"output_config,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
}

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

// wireBlock is one content block. Only the fields relevant to its Type are set.
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
}

// wireImageSource is the `source` object of an Anthropic `image` content block.
// Only the base64 inline form is emitted: type="base64", the image media_type
// (e.g. image/png), and the base64-encoded image bytes in data.
type wireImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type wireTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

// Build converts neutral messages + config into an Anthropic Messages Request.
// The transport must still set Model and/or AnthropicVersion per hosting mode.
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
	for _, m := range messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case "tool":
			// A tool result becomes a user message carrying a tool_result block.
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
			if len(blocks) == 0 {
				continue
			}
			req.Messages = append(req.Messages, wireMessage{Role: "assistant", Content: blocks})
		default: // "user" and anything else
			if len(m.Images) > 0 {
				req.Messages = append(req.Messages, wireMessage{
					Role:    "user",
					Content: imageContentBlocks(m),
				})
				continue
			}
			if m.Content == "" {
				continue
			}
			req.Messages = append(req.Messages, wireMessage{
				Role:    "user",
				Content: []wireBlock{{Type: "text", Text: m.Content}},
			})
		}
	}
	if len(systemParts) > 0 {
		req.System = strings.Join(systemParts, "\n\n")
	}
	return req, nameMap
}

// imageContentBlocks renders a message's text plus its image attachments as an
// Anthropic content-blocks array: a leading text block (only when the message
// carries text), then one base64 `image` block per attachment, in attachment
// order. Each image block's source is {type:"base64", media_type, data} where
// data is the base64-encoding of the raw image bytes.
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
}

type responseBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
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
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			thinking.WriteString(b.Thinking)
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
	role := resp.Role
	if role == "" {
		role = "assistant"
	}
	return modelrepo.ChatResult{
		Message: modelrepo.Message{
			Role:     role,
			Content:  text.String(),
			Thinking: thinking.String(),
		},
		ToolCalls: toolCalls,
	}, nil
}

// newToolCall builds a neutral ToolCall (Function is an anonymous struct).
func newToolCall(id, name, args string) modelrepo.ToolCall {
	tc := modelrepo.ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// streamEvent is the JSON `data:` payload of any Messages SSE event; the `type`
// field discriminates. (The `event:` line is redundant and can be ignored.)
type streamEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		// message_delta carries the final stop_reason here.
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Message struct {
		Usage streamUsage `json:"usage"`
	} `json:"message"`
	Usage streamUsage `json:"usage"`
	// error events carry the failure detail.
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type streamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// StreamDecoder translates streamed Messages SSE events into raw-delta parcels
// (per the modelrepo.StreamParcel contract). It does NOT assemble: tool_use
// blocks surface as ToolCallDelta parcels — id/name from content_block_start,
// argument fragments from input_json_delta — and assembly is left to the
// engine-side modelrepo.StreamAssembler. Anthropic in-stream `error` events
// are decoding errors here so the transport surfaces them as Error parcels
// instead of swallowing them. The stop reason and usage accumulate across
// message_start/message_delta and are surfaced by Finish as the typed
// terminal parcel.
type StreamDecoder struct {
	nameMap    map[string]string
	stopReason string
	usage      modelrepo.TokenUsage
	sawUsage   bool
}

func NewStreamDecoder(nameMap map[string]string) *StreamDecoder {
	return &StreamDecoder{nameMap: nameMap}
}

// DecodeLine parses one SSE `data:` payload (bytes after "data: ") and returns
// the raw-delta parcels it carries, in wire order. An Anthropic `error` event
// returns an error carrying the API's type and message.
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
		if ev.ContentBlock.Type == "tool_use" {
			name := ev.ContentBlock.Name
			if orig, ok := d.nameMap[name]; ok && orig != "" {
				name = orig
			}
			return []*modelrepo.StreamParcel{{ToolCall: &modelrepo.ToolCallDelta{
				Index: ev.Index,
				ID:    ev.ContentBlock.ID,
				Type:  "function",
				Name:  name,
			}}}, nil
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
				return []*modelrepo.StreamParcel{{Thinking: ev.Delta.Thinking}}, nil
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
	default:
		// content_block_stop, message_stop, ping
		return nil, nil
	}
}

func (d *StreamDecoder) recordUsage(u streamUsage) {
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return
	}
	d.sawUsage = true
	if u.InputTokens != 0 {
		d.usage.PromptTokens = u.InputTokens
	}
	if u.OutputTokens != 0 {
		d.usage.CompletionTokens = u.OutputTokens
	}
}

// Finish returns the typed terminal parcel: the stop reason from message_delta
// plus the usage accumulated across message_start/message_delta. Callers emit
// it after the SSE stream ends cleanly.
func (d *StreamDecoder) Finish() *modelrepo.StreamParcel {
	term := &modelrepo.StreamTerminal{FinishReason: d.stopReason}
	if d.sawUsage {
		u := d.usage
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
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
