package vllm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
)

// vllmWireFixture builds the same logical conversation twice: once as it
// looks when produced natively (no provenance fields) and once as it looks
// after a provider migration (history carrying thinking traces and tool-call
// provider_meta such as a Gemini thought_signature).
func vllmWireFixture(withProvenance bool) []modelrepo.Message {
	tc := modelrepo.ToolCall{ID: "call_1", Type: "function"}
	tc.Function.Name = "fs_read"
	tc.Function.Arguments = `{"path":"a.txt"}`
	asst := modelrepo.Message{Role: "assistant", Content: "reading"}
	if withProvenance {
		asst.Thinking = "…internal chain of thought…"
		tc.ProviderMeta = map[string]string{"thought_signature": "sig-bytes"}
	}
	asst.ToolCalls = []modelrepo.ToolCall{tc}
	return []modelrepo.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "read a.txt"},
		asst,
		{Role: "tool", ToolCallID: "call_1", Content: "hello"},
	}
}

func TestUnit_VLLMWire_ProvenanceNeverLeaksAndBytesAreStable(t *testing.T) {
	// P6 fix: history `thinking` and tool-call `provider_meta` must not reach
	// the wire — identical conversations must produce byte-identical request
	// bodies regardless of history provenance, or vLLM's prefix cache misses
	// on semantically identical requests.
	reqA, _ := buildChatRequest("m", vllmWireFixture(false), nil)
	reqB, _ := buildChatRequest("m", vllmWireFixture(true), nil)

	rawA, err := json.Marshal(reqA)
	if err != nil {
		t.Fatal(err)
	}
	rawB, err := json.Marshal(reqB)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawA) != string(rawB) {
		t.Fatalf("request bytes depend on history provenance:\nA: %s\nB: %s", rawA, rawB)
	}
	for _, leak := range []string{"thinking", "provider_meta", "thought_signature", "sig-bytes", "chain of thought"} {
		if strings.Contains(string(rawB), leak) {
			t.Fatalf("provenance field %q leaked onto the wire: %s", leak, rawB)
		}
	}
}

func TestUnit_VLLMWire_KeepsRoleContentToolShape(t *testing.T) {
	// The explicit wire struct must keep the fields vLLM needs: role/content,
	// sanitized tool-call names, and the tool_call_id correlation.
	req, _ := buildChatRequest("m", vllmWireFixture(true), nil)
	raw, err := json.Marshal(req.Messages)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 wire messages, got %d", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "be terse" {
		t.Fatalf("system message malformed: %v", msgs[0])
	}
	tcs, ok := msgs[2]["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("assistant tool_calls missing: %v", msgs[2])
	}
	call := tcs[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Fatalf("tool call malformed: %v", call)
	}
	if msgs[3]["tool_call_id"] != "call_1" {
		t.Fatalf("tool result correlation lost: %v", msgs[3])
	}
}

func TestUnit_VLLMCache_NoWireFieldForHints(t *testing.T) {
	// vLLM's APC keys on the token prefix server-side; CacheHints (and the
	// session key) must not add any bytes to the request.
	plain, _ := buildChatRequest("m", vllmWireFixture(false), nil)
	hinted, _ := buildChatRequest("m", vllmWireFixture(false), []modelrepo.ChatArgument{
		modelrepo.WithCacheHints(modelrepo.CacheHints{
			SessionKey:   "k",
			StableSystem: true,
			StableTools:  true,
		}),
	})
	rawPlain, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	rawHinted, err := json.Marshal(hinted)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawPlain) != string(rawHinted) {
		t.Fatalf("cache hints changed the vLLM request bytes:\n%s\n%s", rawPlain, rawHinted)
	}
}
