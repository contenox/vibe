//go:build llamacpp_direct

package llamacppshim

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectShimBackendInfo(t *testing.T) {
	info := SystemInfo()
	if strings.TrimSpace(info) == "" {
		t.Fatal("SystemInfo returned empty string")
	}
	devices := Devices()
	if len(devices) == 0 {
		t.Fatal("expected at least one ggml backend device")
	}
	t.Logf("system_info=%s", info)
	for _, dev := range devices {
		t.Logf("device[%d] type=%s name=%q desc=%q free=%d total=%d",
			dev.Index, dev.Type, dev.Name, dev.Description, dev.MemoryFree, dev.MemoryTotal)
	}
}

func TestDirectShimInspectModelKVProfileGGUFMetadata(t *testing.T) {
	path := writeShimTestGGUF(t, []func(*bytes.Buffer){
		shimGGUFUint32("qwen2.context_length", 32768),
		shimGGUFUint32("qwen2.block_count", 4),
		shimGGUFUint32("qwen2.attention.head_count_kv", 1),
		shimGGUFUint32("qwen2.attention.head_count", 2),
		shimGGUFUint32("qwen2.attention.key_length", 128),
		shimGGUFUint32("qwen2.embedding_length", 256),
		shimGGUFUint32("qwen2.attention.sliding_window", 512),
		func(b *bytes.Buffer) {
			shimWriteGGUFString(b, "qwen2.attention.sliding_window_pattern")
			_ = binary.Write(b, binary.LittleEndian, uint32(shimGGUFTypeArray))
			_ = binary.Write(b, binary.LittleEndian, uint32(shimGGUFTypeBool))
			_ = binary.Write(b, binary.LittleEndian, uint64(4))
			for _, v := range []uint8{1, 0, 1, 0} {
				_ = binary.Write(b, binary.LittleEndian, v)
			}
		},
	})
	profile, err := InspectModelKVProfile(path)
	if err != nil {
		t.Fatalf("InspectModelKVProfile: %v", err)
	}
	if profile.ContextLength != 32768 ||
		profile.BlockCount != 4 ||
		profile.HeadCountKV != 1 ||
		profile.HeadCount != 2 ||
		profile.KeyLength != 128 ||
		profile.EmbeddingLength != 256 ||
		profile.SlidingWindow != 512 {
		t.Fatalf("profile scalars = %+v", profile)
	}
	want := []bool{true, false, true, false}
	if len(profile.SlidingWindowPattern) != len(want) {
		t.Fatalf("pattern = %+v, want %+v", profile.SlidingWindowPattern, want)
	}
	for i := range want {
		if profile.SlidingWindowPattern[i] != want[i] {
			t.Fatalf("pattern = %+v, want %+v", profile.SlidingWindowPattern, want)
		}
	}
}

func TestDirectShimLoadTinyModel(t *testing.T) {
	path := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	if path == "" {
		t.Skip("set CONTENOX_LLAMA_TINY_GGUF to test direct model load/tokenize")
	}
	model, err := LoadModel(path, ModelConfig{UseMmap: true})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	if got := model.ContextTrain(); got <= 0 {
		t.Fatalf("ContextTrain = %d, want > 0", got)
	}
	toks, err := model.Tokenize("hello from contenox", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) == 0 {
		t.Fatal("Tokenize returned no tokens")
	}
	t.Logf("model=%s n_ctx_train=%d tokens=%v", model.Description(), model.ContextTrain(), toks)
}

func TestDirectShimParseOpenAIToolCalls(t *testing.T) {
	syntax, err := openAIChatToolSyntax(`[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}]`)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseChatResponse(
		`{"tool_calls":[{"id":"call_1","function":{"name":"lookup","arguments":{"query":"x"}}}]}`,
		false,
		syntax,
		"",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Content != "" || parsed.Thinking != "" {
		t.Fatalf("parsed content/thinking = %q/%q, want empty", parsed.Content, parsed.Thinking)
	}
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(parsed.ToolCallsJSON), &calls); err != nil {
		t.Fatalf("tool calls json: %v", err)
	}
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Type != "function" || calls[0].Function.Name != "lookup" || calls[0].Function.Arguments != `{"query":"x"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
}

const (
	shimGGUFTypeUint32 = 4
	shimGGUFTypeBool   = 7
	shimGGUFTypeArray  = 9
)

func shimWriteGGUFString(b *bytes.Buffer, s string) {
	_ = binary.Write(b, binary.LittleEndian, uint64(len(s)))
	b.WriteString(s)
}

func shimGGUFUint32(key string, val uint32) func(*bytes.Buffer) {
	return func(b *bytes.Buffer) {
		shimWriteGGUFString(b, key)
		_ = binary.Write(b, binary.LittleEndian, uint32(shimGGUFTypeUint32))
		_ = binary.Write(b, binary.LittleEndian, val)
	}
}

func writeShimTestGGUF(t *testing.T, kvs []func(*bytes.Buffer)) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("GGUF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(3))
	_ = binary.Write(&b, binary.LittleEndian, uint64(0))
	_ = binary.Write(&b, binary.LittleEndian, uint64(len(kvs)))
	for _, kv := range kvs {
		kv(&b)
	}
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
