package ollama

import (
	"encoding/json"
	"testing"
	"time"
)

// Every expectation here was captured from github.com/ollama/ollama/api
// v0.17.5 before that module was dropped. They are the wire contract: a
// failure means real Ollama servers now see different bytes than they used to,
// which no fake-backed test can catch.

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestDurationMarshalJSON(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, `"0s"`},
		{10 * time.Minute, `"10m0s"`},
		{90 * time.Second, `"1m30s"`},
		{time.Hour + 30*time.Minute, `"1h30m0s"`},
		{1500 * time.Millisecond, `"1.5s"`},
		// Negative is ollama's "keep loaded indefinitely" sentinel and must
		// stay the bare number -1, not a duration string.
		{-1, `-1`},
	}
	for _, tc := range cases {
		if got := mustMarshal(t, Duration{Duration: tc.in}); got != tc.want {
			t.Errorf("Duration(%v): got %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestDurationUnmarshalJSON(t *testing.T) {
	const maxDuration = time.Duration(1<<63 - 1)
	cases := []struct {
		in   string
		want time.Duration
	}{
		// A bare number is seconds, not nanoseconds.
		{`300`, 5 * time.Minute},
		{`0`, 0},
		{`1.5`, 1500 * time.Millisecond},
		{`"10m"`, 10 * time.Minute},
		{`"10m0s"`, 10 * time.Minute},
		// Negative saturates rather than staying negative.
		{`-1`, maxDuration},
		{`"-5s"`, maxDuration},
	}
	for _, tc := range cases {
		var d Duration
		if err := json.Unmarshal([]byte(tc.in), &d); err != nil {
			t.Errorf("Duration %s: unexpected error %v", tc.in, err)
			continue
		}
		if d.Duration != tc.want {
			t.Errorf("Duration %s: got %v, want %v", tc.in, d.Duration, tc.want)
		}
	}

	var d Duration
	if err := json.Unmarshal([]byte(`null`), &d); err == nil {
		t.Error("Duration null: expected an error")
	}
}

func TestThinkValueMarshalJSON(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{true, `true`},
		{false, `false`},
		{"high", `"high"`},
		{"medium", `"medium"`},
		{"low", `"low"`},
		{nil, `null`},
	}
	for _, tc := range cases {
		if got := mustMarshal(t, &ThinkValue{Value: tc.in}); got != tc.want {
			t.Errorf("ThinkValue(%v): got %s, want %s", tc.in, got, tc.want)
		}
	}

	var nilValue *ThinkValue
	if got := mustMarshal(t, nilValue); got != `null` {
		t.Errorf("nil ThinkValue: got %s, want null", got)
	}
}

func TestThinkValueUnmarshalJSON(t *testing.T) {
	for _, in := range []string{`true`, `false`, `"high"`, `"medium"`, `"low"`} {
		var tv ThinkValue
		if err := json.Unmarshal([]byte(in), &tv); err != nil {
			t.Errorf("ThinkValue %s: unexpected error %v", in, err)
			continue
		}
		if got := mustMarshal(t, &tv); got != in {
			t.Errorf("ThinkValue %s: round-tripped to %s", in, got)
		}
	}
	for _, in := range []string{`"bogus"`, `5`, `{}`} {
		var tv ThinkValue
		if err := json.Unmarshal([]byte(in), &tv); err == nil {
			t.Errorf("ThinkValue %s: expected an error", in)
		}
	}
}

func TestImageDataMarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   ImageData
		want string
	}{
		// Base64 of the PNG magic bytes plus a high byte: standard encoding
		// with padding, which is what ollama decodes.
		{"png header", ImageData([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0x00}), `"iVBORw0KGgr/AA=="`},
		{"empty", ImageData{}, `""`},
		{"nil", ImageData(nil), `null`},
	}
	for _, tc := range cases {
		if got := mustMarshal(t, tc.in); got != tc.want {
			t.Errorf("ImageData %s: got %s, want %s", tc.name, got, tc.want)
		}
	}

	var round ImageData
	if err := json.Unmarshal([]byte(`"iVBORw0KGgr/AA=="`), &round); err != nil {
		t.Fatalf("ImageData unmarshal: %v", err)
	}
	if got := mustMarshal(t, round); got != `"iVBORw0KGgr/AA=="` {
		t.Errorf("ImageData round trip: got %s", got)
	}
}

func TestToolCallFunctionArgumentsJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Key order is the model's, not sorted: a plain Go map would emit
		// alpha,mike,zulu here.
		{"preserves key order", `{"zulu":1,"alpha":"a","mike":true}`, `{"zulu":1,"alpha":"a","mike":true}`},
		{"empty object", `{}`, `{}`},
		{"null stays null", `null`, `null`},
		{"numbers", `{"n":1.5,"big":10000000000,"neg":-3}`, `{"n":1.5,"big":10000000000,"neg":-3}`},
		// Only the top level is ordered; nested objects decode to plain maps
		// and re-emit sorted. HTML characters are escaped, as encoding/json does.
		{
			"nested sorts, html escapes",
			`{"nested":{"z":1,"a":2},"arr":[1,"two",null],"esc":"a<b>&c","uni":"héllo"}`,
			`{"nested":{"a":2,"z":1},"arr":[1,"two",null],"esc":"a\u003cb\u003e\u0026c","uni":"héllo"}`,
		},
	}
	for _, tc := range cases {
		var args ToolCallFunctionArguments
		if err := json.Unmarshal([]byte(tc.in), &args); err != nil {
			t.Errorf("%s: unmarshal: %v", tc.name, err)
			continue
		}
		if got := mustMarshal(t, args); got != tc.want {
			t.Errorf("%s: marshal got %s, want %s", tc.name, got, tc.want)
		}
		// String is what feeds modelrepo.ToolCall.Function.Arguments.
		if got := args.String(); got != tc.want {
			t.Errorf("%s: String got %s, want %s", tc.name, got, tc.want)
		}
	}

	// The zero value must never marshal as null: it is a by-value field on
	// every tool call we send.
	var zero ToolCallFunctionArguments
	if got := mustMarshal(t, zero); got != `{}` {
		t.Errorf("zero arguments: got %s, want {}", got)
	}
	if got := zero.String(); got != `{}` {
		t.Errorf("zero arguments String: got %s, want {}", got)
	}
	var nilArgs *ToolCallFunctionArguments
	if got := nilArgs.String(); got != `{}` {
		t.Errorf("nil arguments String: got %s, want {}", got)
	}
}

func TestToolCallMarshalJSON(t *testing.T) {
	var args ToolCallFunctionArguments
	if err := json.Unmarshal([]byte(`{"b":1,"a":2}`), &args); err != nil {
		t.Fatal(err)
	}

	got := mustMarshal(t, ToolCall{ID: "x", Function: ToolCallFunction{Index: 3, Name: "f", Arguments: args}})
	want := `{"id":"x","function":{"index":3,"name":"f","arguments":{"b":1,"a":2}}}`
	if got != want {
		t.Errorf("ToolCall: got %s, want %s", got, want)
	}

	got = mustMarshal(t, ToolCall{Function: ToolCallFunction{Name: "f"}})
	want = `{"function":{"index":0,"name":"f","arguments":{}}}`
	if got != want {
		t.Errorf("ToolCall zero args: got %s, want %s", got, want)
	}
}

func TestToolSchemaJSON(t *testing.T) {
	// Property order is the schema author's; required order is preserved too.
	const schema = `{"type":"object","required":["b","a"],"properties":{"zebra":{"type":"string","description":"z"},"apple":{"type":["string","null"],"enum":["x",1,null]},"nest":{"type":"object","properties":{"inner":{"type":"integer"}}}}}`

	var params ToolFunctionParameters
	if err := json.Unmarshal([]byte(schema), &params); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if got := mustMarshal(t, params); got != schema {
		t.Errorf("ToolFunctionParameters:\n got %s\nwant %s", got, schema)
	}

	got := mustMarshal(t, Tools{{Type: "function", Function: ToolFunction{Name: "n", Description: "d", Parameters: params}}})
	want := `[{"type":"function","function":{"name":"n","description":"d","parameters":` + schema + `}}]`
	if got != want {
		t.Errorf("Tools:\n got %s\nwant %s", got, want)
	}

	// An unset schema still emits the parameters object with a null
	// properties key, as ollama expects.
	got = mustMarshal(t, Tools{{Type: "function", Function: ToolFunction{Name: "n"}}})
	want = `[{"type":"function","function":{"name":"n","parameters":{"type":"","properties":null}}}]`
	if got != want {
		t.Errorf("Tools empty params: got %s, want %s", got, want)
	}
}

func TestPropertyTypeJSON(t *testing.T) {
	// A single type collapses to a bare string; anything else stays an array.
	cases := []string{`"string"`, `["string","null"]`, `[]`}
	for _, in := range cases {
		var pt PropertyType
		if err := json.Unmarshal([]byte(in), &pt); err != nil {
			t.Errorf("PropertyType %s: %v", in, err)
			continue
		}
		if got := mustMarshal(t, pt); got != in {
			t.Errorf("PropertyType %s: round-tripped to %s", in, got)
		}
	}
}

func TestRequestMarshalJSON(t *testing.T) {
	stream := false
	flag := true
	img := []byte{1, 2, 3, 255}
	opts := map[string]any{"temperature": 0.7, "num_predict": 128}

	cases := []struct {
		name string
		in   any
		want string
	}{
		{"ChatRequest zero", ChatRequest{}, `{"model":"","messages":null,"options":null}`},
		{"GenerateRequest zero", GenerateRequest{}, `{"model":"","prompt":"","suffix":"","system":"","template":"","options":null}`},
		{"EmbedRequest zero", EmbedRequest{}, `{"model":"","input":null,"options":null}`},
		{"ShowRequest zero", ShowRequest{}, `{"model":"","system":"","template":"","verbose":false,"options":null,"name":""}`},
		{"DeleteRequest zero", DeleteRequest{}, `{"model":"","name":""}`},
		{
			"ChatRequest full",
			ChatRequest{
				Model:     "m",
				Messages:  []Message{{Role: "user", Content: "hi", Images: []ImageData{img}}},
				Stream:    &stream,
				Format:    json.RawMessage(`{"type":"object"}`),
				KeepAlive: &Duration{Duration: 10 * time.Minute},
				Tools:     Tools{{Type: "function", Function: ToolFunction{Name: "f"}}},
				Options:   opts,
				Think:     &ThinkValue{Value: "low"},
				Truncate:  &flag,
				Shift:     &flag,
			},
			`{"model":"m","messages":[{"role":"user","content":"hi","images":["AQID/w=="]}],"stream":false,"format":{"type":"object"},"keep_alive":"10m0s","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"","properties":null}}}],"options":{"num_predict":128,"temperature":0.7},"think":"low","truncate":true,"shift":true}`,
		},
		{
			"GenerateRequest full",
			GenerateRequest{
				Model: "m", Prompt: "p", System: "s", Stream: &stream, Options: opts,
				Think: &ThinkValue{Value: false}, KeepAlive: &Duration{Duration: 10 * time.Minute},
				Images: []ImageData{img},
			},
			`{"model":"m","prompt":"p","suffix":"","system":"s","template":"","stream":false,"keep_alive":"10m0s","images":["AQID/w=="],"options":{"num_predict":128,"temperature":0.7},"think":false}`,
		},
		{
			"EmbedRequest full",
			EmbedRequest{Model: "m", Input: "text", KeepAlive: &Duration{Duration: 10 * time.Minute}},
			`{"model":"m","input":"text","keep_alive":"10m0s","options":null}`,
		},
	}
	for _, tc := range cases {
		if got := mustMarshal(t, tc.in); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestChatResponseUnmarshalJSON(t *testing.T) {
	const frame = `{"model":"m","created_at":"2024-01-01T00:00:00Z","message":{"role":"ASSISTANT","content":"","tool_calls":[{"function":{"index":0,"name":"f","arguments":{"z":1,"a":"two"}}}]},"done":true,"done_reason":"stop","total_duration":123,"prompt_eval_count":7,"eval_count":9}`

	var resp ChatResponse
	if err := json.Unmarshal([]byte(frame), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Message.UnmarshalJSON lowercases the role.
	if resp.Message.Role != "assistant" {
		t.Errorf("role: got %q, want %q", resp.Message.Role, "assistant")
	}
	// Metrics is embedded untagged, so its counters come from the top level.
	if resp.Metrics.PromptEvalCount != 7 || resp.Metrics.EvalCount != 9 {
		t.Errorf("metrics: got prompt=%d eval=%d, want 7/9", resp.Metrics.PromptEvalCount, resp.Metrics.EvalCount)
	}
	if resp.Metrics.TotalDuration != 123 {
		t.Errorf("total_duration: got %d, want 123", resp.Metrics.TotalDuration)
	}
	if got := resp.Message.ToolCalls[0].Function.Arguments.String(); got != `{"z":1,"a":"two"}` {
		t.Errorf("tool arguments: got %s", got)
	}

	want := `{"model":"m","created_at":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":"","tool_calls":[{"function":{"index":0,"name":"f","arguments":{"z":1,"a":"two"}}}]},"done":true,"done_reason":"stop","total_duration":123,"prompt_eval_count":7,"eval_count":9}`
	if got := mustMarshal(t, resp); got != want {
		t.Errorf("ChatResponse round trip:\n got %s\nwant %s", got, want)
	}
}

func TestGenerateResponseUnmarshalJSON(t *testing.T) {
	const frame = `{"model":"m","created_at":"2024-01-01T00:00:00Z","response":"","done":true,"done_reason":"stop","context":[1,2,3],"total_duration":99,"prompt_eval_count":4,"eval_count":5}`

	var resp GenerateResponse
	if err := json.Unmarshal([]byte(frame), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Metrics.PromptEvalCount != 4 || resp.Metrics.EvalCount != 5 {
		t.Errorf("metrics: got prompt=%d eval=%d, want 4/5", resp.Metrics.PromptEvalCount, resp.Metrics.EvalCount)
	}
	if got := mustMarshal(t, resp); got != frame {
		t.Errorf("GenerateResponse round trip:\n got %s\nwant %s", got, frame)
	}
}

func TestShowAndListResponseJSON(t *testing.T) {
	const show = `{"license":"l","details":{"parent_model":"","format":"gguf","family":"llama","families":["llama"],"parameter_size":"7B","quantization_level":"Q4"},"model_info":{"llama.context_length":4096},"capabilities":["completion","tools","vision","thinking","embedding"],"modified_at":"2024-01-01T00:00:00Z"}`

	var resp ShowResponse
	if err := json.Unmarshal([]byte(show), &resp); err != nil {
		t.Fatalf("unmarshal show: %v", err)
	}
	wantCaps := []Capability{
		CapabilityCompletion, CapabilityTools, CapabilityVision,
		CapabilityThinking, CapabilityEmbedding,
	}
	if len(resp.Capabilities) != len(wantCaps) {
		t.Fatalf("capabilities: got %v", resp.Capabilities)
	}
	for i, want := range wantCaps {
		if resp.Capabilities[i] != want {
			t.Errorf("capability %d: got %q, want %q", i, resp.Capabilities[i], want)
		}
	}
	if got := mustMarshal(t, resp); got != show {
		t.Errorf("ShowResponse round trip:\n got %s\nwant %s", got, show)
	}

	const list = `{"models":[{"name":"a:latest","model":"a:latest","modified_at":"2024-01-01T00:00:00Z","size":123,"digest":"d","details":{"parent_model":"","format":"gguf","family":"llama","families":null,"parameter_size":"7B","quantization_level":"Q4"}}]}`
	var listResp ListResponse
	if err := json.Unmarshal([]byte(list), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if got := mustMarshal(t, listResp); got != list {
		t.Errorf("ListResponse round trip:\n got %s\nwant %s", got, list)
	}

	const embed = `{"model":"m","embeddings":[[0.1,0.2]],"total_duration":5,"prompt_eval_count":3}`
	var embedResp EmbedResponse
	if err := json.Unmarshal([]byte(embed), &embedResp); err != nil {
		t.Fatalf("unmarshal embed: %v", err)
	}
	if got := mustMarshal(t, embedResp); got != embed {
		t.Errorf("EmbedResponse round trip:\n got %s\nwant %s", got, embed)
	}
}

func TestCapabilityConstants(t *testing.T) {
	// These strings are the server's, not ours: renaming one silently
	// misclassifies every model.
	cases := map[Capability]string{
		CapabilityCompletion: "completion",
		CapabilityTools:      "tools",
		CapabilityVision:     "vision",
		CapabilityEmbedding:  "embedding",
		CapabilityThinking:   "thinking",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("capability: got %q, want %q", got, want)
		}
	}
}
