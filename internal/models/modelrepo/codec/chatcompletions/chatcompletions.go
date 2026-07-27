// Package chatcompletions is a transport-agnostic codec for the OpenAI
// Chat Completions wire format (`/chat/completions`-style request/response and
// SSE streaming). It maps between contenox's neutral modelrepo types and the
// OpenAI-compatible JSON shape, performing tool-name sanitization and
// round-tripping.
//
// It does NO I/O: callers build a Request, marshal and POST it through their
// own transport (API-key header for direct OpenAI, bearer token for vLLM), then
// hand the raw response bytes back here to decode. This is what lets each
// OpenAI-compatible provider stay a thin transport wrapper around the shared codec.
package chatcompletions

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/contenox/beam/internal/models/modelrepo"
)

// Request is the OpenAI-compatible chat/completions request body.
//
// Note: this codec emits `max_tokens` (the field every OpenAI-compatible
// endpoint accepts), not the newer `max_completion_tokens`.
type Request struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	Seed        *int          `json:"seed,omitempty"`
	Tools       []wireTool    `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// wireMessage.Content is `any` so it can carry either a plain string (the
// common case, and null for tool-only assistant messages) or the OpenAI
// content-parts array when the message has image attachments.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
}

// wireContentPart is one element of the chat/completions content-parts array,
// used only when a message carries image attachments (a text part plus one
// image_url part per image).
type wireContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL string `json:"url"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireToolDecl `json:"function"`
}

type wireToolDecl struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// Build converts neutral messages + config into a chat/completions Request.
// It returns a nameMap (sanitized tool name -> original) so DecodeResponse /
// the StreamDecoder can translate tool-call names back to what the caller used.
//
// model is placed verbatim in the body; the transport decides the exact string
// (e.g. "gpt-5-mini", or whatever id the provider expects).
func Build(model string, messages []modelrepo.Message, cfg *modelrepo.ChatConfig) (Request, map[string]string) {
	req := Request{Model: model}
	if cfg != nil {
		req.Temperature = cfg.Temperature
		req.MaxTokens = cfg.MaxTokens
		req.TopP = cfg.TopP
		req.Seed = cfg.Seed
	}

	nameMap := make(map[string]string) // sanitized -> original
	origToSanitized := make(map[string]string)
	if cfg != nil && len(cfg.Tools) > 0 {
		seen := map[string]int{}
		tools := make([]wireTool, 0, len(cfg.Tools))
		for i, t := range cfg.Tools {
			if strings.ToLower(t.Type) != "function" || t.Function == nil {
				continue
			}
			orig := t.Function.Name
			name := sanitizeToolName(orig)
			if name == "" {
				name = fmt.Sprintf("tool_%d", i)
			}
			name = uniquifyToolName(seen, name)
			nameMap[name] = orig
			origToSanitized[orig] = name
			tools = append(tools, wireTool{
				Type: "function",
				Function: wireToolDecl{
					Name:        name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				},
			})
		}
		if len(tools) > 0 {
			req.Tools = tools
		}
	}

	req.Messages = make([]wireMessage, 0, len(messages))
	for _, msg := range messages {
		wm := wireMessage{
			Role:       msg.Role,
			ToolCallID: msg.ToolCallID,
		}
		switch {
		case len(msg.Images) > 0:
			// Image attachments force the content-parts array form.
			wm.Content = wireImageContent(msg)
		case msg.Content == "" && len(msg.ToolCalls) > 0:
			// Assistant messages that carry only tool calls send null content.
			wm.Content = nil
		default:
			wm.Content = msg.Content
		}
		for _, tc := range msg.ToolCalls {
			name := tc.Function.Name
			if san, ok := origToSanitized[name]; ok {
				name = san
			} else {
				name = sanitizeToolName(name)
			}
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:       tc.ID,
				Type:     tc.Type,
				Function: wireToolFunction{Name: name, Arguments: tc.Function.Arguments},
			})
		}
		req.Messages = append(req.Messages, wm)
	}

	return req, nameMap
}

// wireImageContent renders a message's text plus its image attachments as the
// content-parts array: a leading text part (when there is text), then one
// image_url part per image, each an inline base64 data URI in attachment order.
func wireImageContent(msg modelrepo.Message) []wireContentPart {
	parts := make([]wireContentPart, 0, len(msg.Images)+1)
	if msg.Content != "" {
		parts = append(parts, wireContentPart{Type: "text", Text: msg.Content})
	}
	for _, img := range msg.Images {
		parts = append(parts, wireContentPart{
			Type:     "image_url",
			ImageURL: &wireImageURL{URL: imageDataURI(img.MimeType, img.Data)},
		})
	}
	return parts
}

// imageDataURI builds the data:<mime>;base64,<payload> URI that every
// OpenAI-compatible vision endpoint accepts for inline image bytes.
func imageDataURI(mimeType string, data []byte) string {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// Response is the non-streaming chat/completions response body.
type Response struct {
	Choices []struct {
		Index        int         `json:"index"`
		Message      responseMsg `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

// wireUsage is the chat-completions usage report. prompt_tokens already
// INCLUDES cached tokens on OpenAI (no normalization needed); the cached
// count is broken out under prompt_tokens_details.cached_tokens. vLLM's V1
// engine reports the details object as null (vllm#44961), which decodes to
// zero — its warm signal is server-side metrics, not per-request usage.
type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *wireUsage) neutralUsage() *modelrepo.TokenUsage {
	if u == nil {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	return &modelrepo.TokenUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      total,
		CacheReadTokens:  u.PromptTokensDetails.CachedTokens,
	}
}

type responseMsg struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

// DecodeResponse parses a non-streaming response into a neutral ChatResult,
// translating sanitized tool-call names back via nameMap.
func DecodeResponse(raw []byte, nameMap map[string]string) (modelrepo.ChatResult, error) {
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return modelrepo.ChatResult{}, fmt.Errorf("chatcompletions: decode response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return modelrepo.ChatResult{}, fmt.Errorf("chatcompletions: no choices in response")
	}
	choice := resp.Choices[0]
	if choice.Message.Content == "" && len(choice.Message.ToolCalls) == 0 && choice.Message.ReasoningContent == "" {
		return modelrepo.ChatResult{}, fmt.Errorf("chatcompletions: empty content (finish_reason=%s)", choice.FinishReason)
	}
	result := modelrepo.ChatResult{
		Message: modelrepo.Message{
			Role:     choice.Message.Role,
			Content:  choice.Message.Content,
			Thinking: choice.Message.ReasoningContent,
		},
		Usage: resp.Usage.neutralUsage(),
	}
	result.ToolCalls = decodeToolCalls(choice.Message.ToolCalls, nameMap)
	return result, nil
}

func decodeToolCalls(in []wireToolCall, nameMap map[string]string) []modelrepo.ToolCall {
	var out []modelrepo.ToolCall
	for _, tc := range in {
		name := tc.Function.Name
		if orig, ok := nameMap[name]; ok && orig != "" {
			name = orig
		}
		out = append(out, newToolCall(tc.ID, tc.Type, name, tc.Function.Arguments))
	}
	return out
}

// newToolCall builds a neutral ToolCall. The Function field is an anonymous
// struct on modelrepo.ToolCall, so it is constructed via this helper.
func newToolCall(id, typ, name, args string) modelrepo.ToolCall {
	tc := modelrepo.ToolCall{ID: id, Type: typ}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// streamChunk is one SSE chunk of a streamed chat/completions response.
// The Reasoning field is a vLLM variant spelling of reasoning_content; both
// map to a Thinking delta.
type streamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

// StreamDecoder translates streamed chat/completions chunks into raw-delta
// parcels (per the modelrepo.StreamParcel contract). It does NOT assemble:
// tool-call fragments are emitted as ToolCallDelta parcels (names translated
// through nameMap as they appear) and assembly is left to the engine-side
// modelrepo.StreamAssembler. The finish reason and usage report are held back
// and surfaced by Finish as the typed terminal parcel, because the wire can
// deliver a trailing usage-only chunk after the finish_reason chunk.
type StreamDecoder struct {
	nameMap      map[string]string
	finishReason string
	usage        *modelrepo.TokenUsage
}

// NewStreamDecoder returns a decoder. nameMap is the sanitized->original map
// from Build (may be nil if no tools).
func NewStreamDecoder(nameMap map[string]string) *StreamDecoder {
	return &StreamDecoder{nameMap: nameMap}
}

// DecodeLine parses one SSE data payload (the bytes AFTER the "data: " prefix,
// excluding the "[DONE]" sentinel which the caller should skip) and returns
// the raw-delta parcels it carries, in wire order.
func (d *StreamDecoder) DecodeLine(payload []byte) ([]*modelrepo.StreamParcel, error) {
	var chunk streamChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, fmt.Errorf("chatcompletions: decode stream chunk: %w", err)
	}
	if chunk.Usage != nil {
		d.usage = chunk.Usage.neutralUsage()
	}
	if len(chunk.Choices) == 0 {
		return nil, nil
	}
	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		d.finishReason = choice.FinishReason
	}

	var parcels []*modelrepo.StreamParcel
	if choice.Delta.Content != "" {
		parcels = append(parcels, &modelrepo.StreamParcel{Data: choice.Delta.Content})
	}
	if thinking := firstNonEmpty(choice.Delta.Reasoning, choice.Delta.ReasoningContent); thinking != "" {
		parcels = append(parcels, &modelrepo.StreamParcel{Thinking: thinking})
	}
	for _, tc := range choice.Delta.ToolCalls {
		name := tc.Function.Name
		if orig, ok := d.nameMap[name]; ok && orig != "" {
			name = orig
		}
		parcels = append(parcels, &modelrepo.StreamParcel{ToolCall: &modelrepo.ToolCallDelta{
			Index:        tc.Index,
			ID:           tc.ID,
			Type:         tc.Type,
			Name:         name,
			ArgsFragment: tc.Function.Arguments,
		}})
	}
	return parcels, nil
}

// Finish returns the typed terminal parcel: the finish reason last seen on the
// wire plus the usage report when the provider sent one. Callers emit it after
// the SSE stream ends cleanly ([DONE] or EOF without error).
func (d *StreamDecoder) Finish() *modelrepo.StreamParcel {
	return &modelrepo.StreamParcel{Terminal: &modelrepo.StreamTerminal{
		FinishReason: d.finishReason,
		Usage:        d.usage,
	}}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sanitizeToolName replaces characters outside OpenAI's allowed set
// (^[a-zA-Z0-9_-]+$) with '_' and trims leading/trailing separators.
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
	return strings.Trim(b.String(), "_-")
}

func uniquifyToolName(seen map[string]int, name string) string {
	if _, ok := seen[name]; !ok {
		seen[name] = 1
		return name
	}
	i := seen[name]
	for {
		candidate := fmt.Sprintf("%s_%d", name, i)
		if _, ok := seen[candidate]; !ok {
			seen[name] = i + 1
			seen[candidate] = 1
			return candidate
		}
		i++
	}
}
