//go:build llamanode && llamacpp_direct

package llamasession

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/transport"
)

// toolCallEnvelopeSchema mirrors the runtime's llama:json_schema_tool_calls
// payload: an envelope whose tool_calls entries pin the tool name via enum.
// The location argument is enum-bounded so even a 5M-parameter model must
// finish a complete, valid call within a small token budget — conformance
// comes from the grammar, not the model.
const toolCallEnvelopeSchema = `{
  "type": "object",
  "properties": {
    "tool_calls": {
      "type": "array",
      "minItems": 1,
      "maxItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "enum": ["get_weather"]},
          "arguments": {
            "type": "object",
            "properties": {"location": {"type": "string", "enum": ["berlin", "paris"]}},
            "required": ["location"],
            "additionalProperties": false
          }
        },
        "required": ["name", "arguments"],
        "additionalProperties": false
      }
    }
  },
  "required": ["tool_calls"],
  "additionalProperties": false
}`

// TestSystem_LlamaSessionStructuredToolCalls proves the GBNF structured-output
// path end-to-end on a real model: schema → grammar → constrained decode →
// parsed transport tool calls.
func TestSystem_LlamaSessionStructuredToolCalls(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	requireTinyGGUF(t, modelPath)

	sess, err := New(modelPath, llama.Config{NumCtx: 512, NumBatch: 64})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	ctx := context.Background()

	if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: "You can call tools. "}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: "What is the weather in berlin?"}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}

	temp := 0.0
	ch, err := sess.Decode(ctx, llama.DecodeConfig{
		MaxTokens:   256,
		Temperature: &temp,
		StructuredOutput: transport.StructuredOutputConfig{
			Protocol: "llama:json_schema_tool_calls",
			Payload:  toolCallEnvelopeSchema,
		},
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var calls []transport.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("decode chunk error: %v", chunk.Error)
		}
		calls = append(calls, chunk.ToolCalls...)
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v, want exactly one", calls)
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("tool name = %q, want get_weather (enum-constrained)", calls[0].Function.Name)
	}
	var args struct {
		Location string `json:"location"`
	}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments %q not valid JSON: %v", calls[0].Function.Arguments, err)
	}
	if args.Location != "berlin" && args.Location != "paris" {
		t.Fatalf("location = %q, want enum member", args.Location)
	}
	t.Logf("constrained tool call: %s(%s)", calls[0].Function.Name, calls[0].Function.Arguments)
}

// TestSystem_LlamaSessionStructuredJSONSchema proves the generic json_schema
// protocol: constrained streaming text that must parse as schema-conforming
// JSON regardless of model quality.
func TestSystem_LlamaSessionStructuredJSONSchema(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	requireTinyGGUF(t, modelPath)

	sess, err := New(modelPath, llama.Config{NumCtx: 512, NumBatch: 64})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	ctx := context.Background()

	if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: "Answer in JSON. "}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: "Pick a city."}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}

	temp := 0.0
	ch, err := sess.Decode(ctx, llama.DecodeConfig{
		MaxTokens:   64,
		Temperature: &temp,
		StructuredOutput: transport.StructuredOutputConfig{
			Protocol: "json_schema",
			Payload:  `{"type":"object","properties":{"city":{"type":"string","enum":["berlin","paris"]}},"required":["city"],"additionalProperties":false}`,
		},
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var out strings.Builder
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("decode chunk error: %v", chunk.Error)
		}
		out.WriteString(chunk.Text)
	}
	var parsed struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal([]byte(out.String()), &parsed); err != nil {
		t.Fatalf("constrained output %q is not valid JSON: %v", out.String(), err)
	}
	if parsed.City != "berlin" && parsed.City != "paris" {
		t.Fatalf("city = %q, want enum member", parsed.City)
	}
	t.Logf("constrained JSON: %s", out.String())
}
