package sessionkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/contenox/contenox/libtransport"
)

// ParsedToolCall is the permissive wire shape of one model-emitted tool call.
// Models and native parsers disagree on field names (name/tool_name/function,
// arguments/parameters), so it accepts all spellings; TransportToolCalls
// normalizes them onto the backend-neutral transport.ToolCall. Shared by both
// backend adapters so structured tool-call output parses identically
// regardless of which engine constrained the generation.
type ParsedToolCall struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Name       string          `json:"name"`
	ToolName   string          `json:"tool_name"`
	Arguments  json.RawMessage `json:"arguments"`
	Parameters json.RawMessage `json:"parameters"`
	Function   struct {
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// TransportToolCalls normalizes permissive tool-call shapes onto
// transport.ToolCall, defaulting IDs and the "function" type.
func TransportToolCalls(in []ParsedToolCall) ([]transport.ToolCall, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]transport.ToolCall, 0, len(in))
	for i, tc := range in {
		call := transport.ToolCall{ID: tc.ID, Type: tc.Type}
		if call.ID == "" {
			call.ID = fmt.Sprintf("call_%d", i+1)
		}
		if call.Type == "" {
			call.Type = "function"
		}
		call.Function.Name = tc.Function.Name
		if call.Function.Name == "" {
			call.Function.Name = tc.Name
		}
		if call.Function.Name == "" {
			call.Function.Name = tc.ToolName
		}
		rawArgs := tc.Function.Arguments
		if len(rawArgs) == 0 {
			rawArgs = tc.Function.Parameters
		}
		if len(rawArgs) == 0 {
			rawArgs = tc.Arguments
		}
		if len(rawArgs) == 0 {
			rawArgs = tc.Parameters
		}
		args, err := normalizeToolArguments(rawArgs)
		if err != nil {
			return nil, err
		}
		call.Function.Arguments = args
		out = append(out, call)
	}
	return out, nil
}

// StructuredToolCallChunk parses the complete text of a structured tool-call
// generation into a StreamChunk carrying transport tool calls. It accepts the
// two shapes constrained decoding produces: a JSON envelope
// ({"content":…,"tool_calls":[…]} or a bare call object) and Qwen-style
// <tool_call>…</tool_call> tag blocks.
func StructuredToolCallChunk(text string) (transport.StreamChunk, error) {
	raw := bytes.TrimSpace([]byte(text))
	if len(raw) == 0 {
		return transport.StreamChunk{}, fmt.Errorf("structured tool call output is empty")
	}
	if raw[0] != '{' {
		return chunkFromQwenToolCallTags(text)
	}

	var envelope struct {
		Content   *string          `json:"content"`
		ToolCalls []ParsedToolCall `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return transport.StreamChunk{}, fmt.Errorf("parse structured tool call envelope: %w", err)
	}
	chunk := transport.StreamChunk{}
	if envelope.Content != nil {
		chunk.Text = *envelope.Content
	}
	if len(envelope.ToolCalls) == 0 {
		// Bare objects without the envelope ({"function":…}) are deliberately
		// rejected: constrained schemas emit {"content":…} or {"tool_calls":…},
		// and silently accepting legacy shapes would mask schema drift.
		if envelope.Content == nil {
			return transport.StreamChunk{}, fmt.Errorf("structured tool call envelope contained neither content nor tool_calls")
		}
		return chunk, nil
	}
	calls, err := TransportToolCalls(envelope.ToolCalls)
	if err != nil {
		return transport.StreamChunk{}, err
	}
	chunk.ToolCalls = calls
	return chunk, nil
}

var qwenToolCallTagRE = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

func chunkFromQwenToolCallTags(text string) (transport.StreamChunk, error) {
	matches := qwenToolCallTagRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return transport.StreamChunk{Text: strings.TrimSpace(text)}, nil
	}

	var remaining strings.Builder
	toolCalls := make([]ParsedToolCall, 0, len(matches))
	last := 0
	for _, match := range matches {
		remaining.WriteString(text[last:match[0]])
		last = match[1]

		rawCall := strings.TrimSpace(text[match[2]:match[3]])
		var parsed struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(rawCall), &parsed); err != nil {
			return transport.StreamChunk{}, fmt.Errorf("parse qwen tool_call payload: %w", err)
		}
		if parsed.Name == "" {
			return transport.StreamChunk{}, fmt.Errorf("qwen tool_call payload missing name")
		}
		toolCalls = append(toolCalls, ParsedToolCall{
			ID:        fmt.Sprintf("call_%d", len(toolCalls)+1),
			Type:      "function",
			Name:      parsed.Name,
			Arguments: parsed.Arguments,
		})
	}
	remaining.WriteString(text[last:])

	calls, err := TransportToolCalls(toolCalls)
	if err != nil {
		return transport.StreamChunk{}, err
	}
	return transport.StreamChunk{
		Text:      strings.TrimSpace(remaining.String()),
		ToolCalls: calls,
	}, nil
}

func normalizeToolArguments(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "{}", nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("parse tool arguments string: %w", err)
		}
		if strings.TrimSpace(s) == "" {
			return "{}", nil
		}
		return s, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", fmt.Errorf("compact tool arguments: %w", err)
	}
	return compact.String(), nil
}
