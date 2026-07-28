package messages

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/models/modelrepo"
)

func cacheFixtureMessages() []modelrepo.Message {
	asst := modelrepo.Message{Role: "assistant", Content: "using the tool"}
	tc := modelrepo.ToolCall{ID: "call_1", Type: "function"}
	tc.Function.Name = "fs.read"
	tc.Function.Arguments = `{"path":"a.txt"}`
	asst.ToolCalls = []modelrepo.ToolCall{tc}
	return []modelrepo.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "read a.txt"},
		asst,
		{Role: "tool", ToolCallID: "call_1", Content: "hello"},
		{Role: "user", Content: "and now?"},
	}
}

func cacheFixtureConfig(hints *modelrepo.CacheHints) *modelrepo.ChatConfig {
	return &modelrepo.ChatConfig{
		Tools: []modelrepo.Tool{
			{Type: "function", Function: &modelrepo.FunctionTool{Name: "fs.read"}},
			{Type: "function", Function: &modelrepo.FunctionTool{Name: "fs.write"}},
		},
		CacheHints: hints,
	}
}

// stripCacheMetadata removes every cache_control key from a decoded request
// and normalizes the system field to its text content, so hinted and unhinted
// requests can be compared for model-visible equality.
func stripCacheMetadata(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	removeCacheControl(m)
	if sys, ok := m["system"].([]any); ok {
		var parts []string
		for _, b := range sys {
			blk, ok := b.(map[string]any)
			if !ok || blk["type"] != "text" {
				t.Fatalf("system block form must contain only text blocks, got %v", b)
			}
			parts = append(parts, blk["text"].(string))
		}
		m["system"] = strings.Join(parts, "\n\n")
	}
	return m
}

func removeCacheControl(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "cache_control")
		for _, vv := range x {
			removeCacheControl(vv)
		}
	case []any:
		for _, vv := range x {
			removeCacheControl(vv)
		}
	}
}

func countCacheControls(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["cache_control"]; ok {
			n++
		}
		for _, vv := range x {
			n += countCacheControls(vv)
		}
	case []any:
		for _, vv := range x {
			n += countCacheControls(vv)
		}
	}
	return n
}

func TestUnit_CacheHints_AbsentIsByteIdenticalPreChangeShape(t *testing.T) {
	// No hints: request keeps the pre-cache wire shape — plain-string system, zero cache_control keys.
	req, _ := Build(cacheFixtureMessages(), cacheFixtureConfig(nil))
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cache_control") {
		t.Fatalf("hint-less request must carry no cache metadata: %s", raw)
	}
	sys, ok := req.System.(string)
	if !ok || sys != "be terse" {
		t.Fatalf("hint-less request must keep the plain-string system form, got %#v", req.System)
	}
}

func TestUnit_CacheHints_PlacementToolsSystemHistory(t *testing.T) {
	hints := &modelrepo.CacheHints{
		StableSystem:     true,
		StableTools:      true,
		StableHistoryLen: 4, // system + user + assistant + tool of the fixture
	}
	req, _ := Build(cacheFixtureMessages(), cacheFixtureConfig(hints))

	// Breakpoint 1: the last tool definition, and only it.
	if req.Tools[0].CacheControl != nil {
		t.Fatalf("cache_control must sit on the last tool only")
	}
	last := req.Tools[len(req.Tools)-1]
	if last.CacheControl == nil || last.CacheControl.Type != "ephemeral" || last.CacheControl.TTL != "" {
		t.Fatalf("last tool must carry {type:ephemeral} without TTL, got %+v", last.CacheControl)
	}

	// Breakpoint 2: system rendered as one text block with cache_control, same text as the string form.
	blocks, ok := req.System.([]wireBlock)
	if !ok || len(blocks) != 1 {
		t.Fatalf("hinted system must be a single text block, got %#v", req.System)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "be terse" || blocks[0].CacheControl == nil {
		t.Fatalf("system block malformed: %+v", blocks[0])
	}

	// Breakpoint 3: StableHistoryLen=4 covers neutral messages 0..3; the last
	// wire message from that prefix is the tool_result message (wire index 2,
	// since system is hoisted). The trailing user turn stays unmarked.
	if got := req.Messages[2].Content[len(req.Messages[2].Content)-1].CacheControl; got == nil {
		t.Fatalf("stable-history breakpoint missing on wire message 2")
	}
	lastMsg := req.Messages[len(req.Messages)-1]
	if lastMsg.Content[len(lastMsg.Content)-1].CacheControl != nil {
		t.Fatalf("volatile tail must not carry a breakpoint")
	}

	// Documented limit: never more than MaxCacheBreakpoints markers total.
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if n := countCacheControls(decoded); n != 3 || n > MaxCacheBreakpoints {
		t.Fatalf("expected exactly 3 breakpoints (≤%d), got %d", MaxCacheBreakpoints, n)
	}
}

func TestUnit_CacheHints_NeverChangeModelVisibleContent(t *testing.T) {
	// Hinted and unhinted requests must be identical modulo cache metadata.
	plain, _ := Build(cacheFixtureMessages(), cacheFixtureConfig(nil))
	hinted, _ := Build(cacheFixtureMessages(), cacheFixtureConfig(&modelrepo.CacheHints{
		StableSystem:     true,
		StableTools:      true,
		StableHistoryLen: 4,
		TTL:              time.Hour,
	}))

	rawPlain, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	rawHinted, err := json.Marshal(hinted)
	if err != nil {
		t.Fatal(err)
	}

	normPlain, _ := json.Marshal(stripCacheMetadata(t, rawPlain))
	normHinted, _ := json.Marshal(stripCacheMetadata(t, rawHinted))
	if string(normPlain) != string(normHinted) {
		t.Fatalf("hints changed model-visible content:\nplain:  %s\nhinted: %s", normPlain, normHinted)
	}
}

func TestUnit_CacheHints_TTLMapsToOneHourVariant(t *testing.T) {
	req, _ := Build(cacheFixtureMessages(), cacheFixtureConfig(&modelrepo.CacheHints{
		StableTools: true,
		TTL:         time.Hour,
	}))
	last := req.Tools[len(req.Tools)-1]
	if last.CacheControl == nil || last.CacheControl.TTL != "1h" {
		t.Fatalf("TTL ≥ 1h must map to the documented 1h variant, got %+v", last.CacheControl)
	}
	// Sub-hour TTLs use the 5m default (field omitted).
	req5, _ := Build(cacheFixtureMessages(), cacheFixtureConfig(&modelrepo.CacheHints{
		StableTools: true,
		TTL:         5 * time.Minute,
	}))
	if got := req5.Tools[len(req5.Tools)-1].CacheControl; got == nil || got.TTL != "" {
		t.Fatalf("sub-hour TTL must omit the ttl field, got %+v", got)
	}
}

func TestUnit_CacheHints_NoContentNoBreakpoint(t *testing.T) {
	// Hints asserting stability over absent content place nothing.
	req, _ := Build([]modelrepo.Message{{Role: "user", Content: "hi"}}, &modelrepo.ChatConfig{
		CacheHints: &modelrepo.CacheHints{StableSystem: true, StableTools: true},
	})
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cache_control") {
		t.Fatalf("no tools and no system: nothing to mark, got %s", raw)
	}
}

func TestUnit_CacheUsage_NormalizationRule(t *testing.T) {
	// Anthropic input_tokens excludes cache reads/writes; PromptTokens is the sum of the three.
	body := `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":7,"output_tokens":11,"cache_read_input_tokens":100,"cache_creation_input_tokens":30}}`
	res, err := DecodeResponse([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	u := res.Usage
	if u == nil {
		t.Fatal("usage must be extracted from the non-streaming response")
	}
	if u.PromptTokens != 137 || u.CacheReadTokens != 100 || u.CacheWriteTokens != 30 {
		t.Fatalf("normalization violated: %+v", u)
	}
	if u.CompletionTokens != 11 || u.TotalTokens != 148 {
		t.Fatalf("completion/total wrong: %+v", u)
	}
}

func TestUnit_StreamDecoder_CacheUsageAcrossStartAndDelta(t *testing.T) {
	d := NewStreamDecoder(nil)
	events := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":200,"cache_creation_input_tokens":50}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
	}
	for _, ev := range events {
		if _, err := d.DecodeLine([]byte(ev)); err != nil {
			t.Fatal(err)
		}
	}
	term := d.Finish().Terminal
	if term == nil || term.Usage == nil {
		t.Fatal("terminal usage missing")
	}
	u := term.Usage
	if u.PromptTokens != 255 || u.CacheReadTokens != 200 || u.CacheWriteTokens != 50 || u.CompletionTokens != 9 {
		t.Fatalf("stream usage normalization violated: %+v", u)
	}
}
