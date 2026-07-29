//go:build llamacpp_direct

package llamacppshim

/*
#cgo CFLAGS: -std=c11
#cgo CXXFLAGS: -std=c++17
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include "gguf.h"
#include "llama.h"
#include "ggml-backend.h"

struct cx_chat_apply_result {
    char *prompt;
    char *parser;
    char *generation_prompt;
    int format;
};

struct cx_chat_syntax_result {
    char *parser;
    int format;
};

struct cx_chat_apply_result cx_common_chat_apply(const struct llama_model *model,
                                                 const char *messages_json,
                                                 const char *tools_json,
                                                 int add_generation_prompt,
                                                 const char *reasoning_format,
                                                 int enable_thinking,
                                                 const char *reasoning_effort,
                                                 char *errbuf,
                                                 size_t errlen);

struct cx_chat_probe_result {
    char *format_name;
    char *thinking_start_tag;
    int supports_tool_calls;
    int supports_thinking;
    int supports_reasoning_effort;
};

struct cx_chat_probe_result cx_probe_chat_template(const char *model_path,
                                                   char *errbuf,
                                                   size_t errlen);

int cx_common_chat_parse(const char *input,
                         int is_partial,
                         int format,
                         const char *parser_json,
                         const char *generation_prompt,
                         const char *reasoning_format,
                         int parse_tool_calls,
                         char **content_out,
                         char **reasoning_out,
                         char **tool_calls_out,
                         char *errbuf,
                         size_t errlen);

struct cx_chat_syntax_result cx_common_chat_syntax_openai_tools(const char *tools_json,
                                                                char *errbuf,
                                                                size_t errlen);

int cx_json_schema_to_grammar(const char *schema_json, char **out);
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// backendDirEnv names the directory holding the ggml backend plugins
// (libggml-cpu-*.so, libggml-cuda.so, …). With GGML_BACKEND_DL=ON the plugins are
// dlopen'd at runtime rather than linked, and ggml otherwise only searches the
// executable directory and the cwd — neither of which is the bundled lib dir.
// Setting this env var points the loader at the right place (the packaged
// wrapper and `make run-modeld*` targets set it; see mk/llama-flags.mk).
const backendDirEnv = "CONTENOX_LLAMA_BACKEND_DIR"

// Available reports whether the direct llama.cpp shim was compiled in.
const Available = true

var backendOnce sync.Once

func initBackend() {
	backendOnce.Do(func() {
		if dir := strings.TrimSpace(os.Getenv(backendDirEnv)); dir != "" {
			cdir := C.CString(dir)
			defer C.free(unsafe.Pointer(cdir))
			C.ggml_backend_load_all_from_path(cdir)
		} else {
			C.ggml_backend_load_all()
		}
		C.llama_backend_init()
	})
}

// SystemInfo returns llama.cpp's linked runtime system information string.
func SystemInfo() string {
	initBackend()
	return C.GoString(C.llama_print_system_info())
}

// SupportsGPUOffload reports the linked llama.cpp runtime's compiled GPU
// offload capability. A true result is not enough to certify actual placement.
func SupportsGPUOffload() bool {
	initBackend()
	return bool(C.llama_supports_gpu_offload())
}

// Device describes a ggml backend device registered in the linked runtime.
type Device struct {
	Index       int
	Name        string
	Description string
	Type        string
	MemoryFree  uint64
	MemoryTotal uint64
}

// Devices returns the ggml backend devices registered after backend init.
func Devices() []Device {
	initBackend()
	count := int(C.ggml_backend_dev_count())
	out := make([]Device, 0, count)
	for i := 0; i < count; i++ {
		dev := C.ggml_backend_dev_get(C.size_t(i))
		if dev == nil {
			continue
		}
		var free, total C.size_t
		C.ggml_backend_dev_memory(dev, &free, &total)
		out = append(out, Device{
			Index:       i,
			Name:        C.GoString(C.ggml_backend_dev_name(dev)),
			Description: C.GoString(C.ggml_backend_dev_description(dev)),
			Type:        deviceType(C.ggml_backend_dev_type(dev)),
			MemoryFree:  uint64(free),
			MemoryTotal: uint64(total),
		})
	}
	return out
}

func deviceType(t C.enum_ggml_backend_dev_type) string {
	switch t {
	case C.GGML_BACKEND_DEVICE_TYPE_CPU:
		return "cpu"
	case C.GGML_BACKEND_DEVICE_TYPE_GPU:
		return "gpu"
	case C.GGML_BACKEND_DEVICE_TYPE_IGPU:
		return "igpu"
	case C.GGML_BACKEND_DEVICE_TYPE_ACCEL:
		return "accel"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// ModelKVProfile contains model metadata needed to budget KV cache. It is read
// through llama.cpp/ggml public GGUF APIs, without model tensor allocation.
type ModelKVProfile struct {
	ContextLength              int
	BlockCount                 int
	HeadCountKV                int
	HeadCount                  int
	KeyLength                  int
	EmbeddingLength            int
	SlidingWindow              int
	SlidingWindowPattern       []bool
	SlidingWindowPatternStride int
}

// InspectModelKVProfile reads GGUF metadata through the public gguf.h API. It
// does not load tensor data.
func InspectModelKVProfile(path string) (ModelKVProfile, error) {
	if path == "" {
		return ModelKVProfile{}, errors.New("llamacppshim: model path is required")
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	params := C.struct_gguf_init_params{no_alloc: C.bool(true)}
	ctx := C.gguf_init_from_file(cpath, params)
	if ctx == nil {
		return ModelKVProfile{}, fmt.Errorf("llamacppshim: inspect GGUF metadata %q", path)
	}
	defer C.gguf_free(ctx)

	var p ModelKVProfile
	nKV := int(C.gguf_get_n_kv(ctx))
	for i := 0; i < nKV; i++ {
		key := C.GoString(C.gguf_get_key(ctx, C.int64_t(i)))
		id := C.int64_t(i)
		switch {
		case strings.HasSuffix(key, ".context_length"):
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.ContextLength = int(v)
			}
		case strings.HasSuffix(key, ".block_count"):
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.BlockCount = int(v)
			}
		case strings.HasSuffix(key, ".attention.head_count_kv"):
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.HeadCountKV = int(v)
			}
		case strings.HasSuffix(key, ".attention.head_count"):
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.HeadCount = int(v)
			}
		case strings.HasSuffix(key, ".attention.key_length"):
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.KeyLength = int(v)
			}
		case strings.HasSuffix(key, ".attention.sliding_window"):
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.SlidingWindow = int(v)
			}
		case strings.HasSuffix(key, ".attention.sliding_window_pattern"):
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.SlidingWindowPatternStride = int(v)
			} else if pattern := ggufBoolPatternArray(ctx, id); len(pattern) > 0 {
				p.SlidingWindowPattern = pattern
			}
		case strings.HasSuffix(key, ".embedding_length"):
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.EmbeddingLength = int(v)
			}
		}
	}
	return p, nil
}

// MMProjProfile contains the multimodal projector metadata needed to estimate
// per-image token cost. Zero means absent.
type MMProjProfile struct {
	ImageSize       int // clip.vision.image_size
	PatchSize       int // clip.vision.patch_size
	ProjScaleFactor int // clip.vision.projector.scale_factor (pixel-shuffle merge)
}

// InspectMMProjProfile reads projector GGUF metadata through the public gguf.h
// API. It does not load tensor data.
func InspectMMProjProfile(path string) (MMProjProfile, error) {
	if path == "" {
		return MMProjProfile{}, errors.New("llamacppshim: mmproj path is required")
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	params := C.struct_gguf_init_params{no_alloc: C.bool(true)}
	ctx := C.gguf_init_from_file(cpath, params)
	if ctx == nil {
		return MMProjProfile{}, fmt.Errorf("llamacppshim: inspect GGUF metadata %q", path)
	}
	defer C.gguf_free(ctx)

	var p MMProjProfile
	nKV := int(C.gguf_get_n_kv(ctx))
	for i := 0; i < nKV; i++ {
		key := C.GoString(C.gguf_get_key(ctx, C.int64_t(i)))
		id := C.int64_t(i)
		switch key {
		case "clip.vision.image_size":
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.ImageSize = int(v)
			}
		case "clip.vision.patch_size":
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.PatchSize = int(v)
			}
		case "clip.vision.projector.scale_factor":
			if v, ok := ggufScalarInt(ctx, id); ok {
				p.ProjScaleFactor = int(v)
			}
		}
	}
	return p, nil
}

func ggufScalarInt(ctx *C.struct_gguf_context, id C.int64_t) (int64, bool) {
	switch C.gguf_get_kv_type(ctx, id) {
	case C.GGUF_TYPE_UINT8:
		return int64(C.gguf_get_val_u8(ctx, id)), true
	case C.GGUF_TYPE_INT8:
		return int64(C.gguf_get_val_i8(ctx, id)), true
	case C.GGUF_TYPE_UINT16:
		return int64(C.gguf_get_val_u16(ctx, id)), true
	case C.GGUF_TYPE_INT16:
		return int64(C.gguf_get_val_i16(ctx, id)), true
	case C.GGUF_TYPE_UINT32:
		return int64(C.gguf_get_val_u32(ctx, id)), true
	case C.GGUF_TYPE_INT32:
		return int64(C.gguf_get_val_i32(ctx, id)), true
	case C.GGUF_TYPE_UINT64:
		return int64(C.gguf_get_val_u64(ctx, id)), true
	case C.GGUF_TYPE_INT64:
		return int64(C.gguf_get_val_i64(ctx, id)), true
	case C.GGUF_TYPE_BOOL:
		if bool(C.gguf_get_val_bool(ctx, id)) {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func ggufBoolPatternArray(ctx *C.struct_gguf_context, id C.int64_t) []bool {
	if C.gguf_get_kv_type(ctx, id) != C.GGUF_TYPE_ARRAY {
		return nil
	}
	n := int(C.gguf_get_arr_n(ctx, id))
	if n <= 0 {
		return nil
	}
	data := C.gguf_get_arr_data(ctx, id)
	if data == nil {
		return nil
	}
	out := make([]bool, n)
	switch C.gguf_get_arr_type(ctx, id) {
	case C.GGUF_TYPE_BOOL, C.GGUF_TYPE_UINT8, C.GGUF_TYPE_INT8:
		vals := unsafe.Slice((*C.int8_t)(data), n)
		for i, v := range vals {
			out[i] = v != 0
		}
	case C.GGUF_TYPE_UINT16, C.GGUF_TYPE_INT16:
		vals := unsafe.Slice((*C.int16_t)(data), n)
		for i, v := range vals {
			out[i] = v != 0
		}
	case C.GGUF_TYPE_UINT32, C.GGUF_TYPE_INT32:
		vals := unsafe.Slice((*C.int32_t)(data), n)
		for i, v := range vals {
			out[i] = v != 0
		}
	case C.GGUF_TYPE_UINT64, C.GGUF_TYPE_INT64:
		vals := unsafe.Slice((*C.int64_t)(data), n)
		for i, v := range vals {
			out[i] = v != 0
		}
	default:
		return nil
	}
	return out
}

// ModelConfig controls model loading for the direct shim.
type ModelConfig struct {
	NumGPULayers int
	TensorSplit  []float32
	UseMmap      bool
	VocabOnly    bool
}

// Model wraps a direct llama_model pointer.
type Model struct {
	ptr   *C.struct_llama_model
	vocab *C.struct_llama_vocab
}

// LoadModel opens a GGUF model through direct llama.cpp.
func LoadModel(path string, cfg ModelConfig) (*Model, error) {
	if path == "" {
		return nil, errors.New("llamacppshim: model path is required")
	}
	initBackend()
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	params := C.llama_model_default_params()
	params.n_gpu_layers = C.int32_t(cfg.NumGPULayers)
	params.use_mmap = C.bool(cfg.UseMmap)
	params.vocab_only = C.bool(cfg.VocabOnly)
	if len(cfg.TensorSplit) > 0 {
		var pin runtime.Pinner
		pin.Pin(&cfg.TensorSplit[0])
		defer pin.Unpin()
		params.tensor_split = (*C.float)(unsafe.Pointer(&cfg.TensorSplit[0]))
	}

	ptr := C.llama_model_load_from_file(cpath, params)
	if ptr == nil {
		return nil, fmt.Errorf("llamacppshim: load model %q", path)
	}
	m := &Model{ptr: ptr, vocab: C.llama_model_get_vocab(ptr)}
	runtime.SetFinalizer(m, (*Model).Close)
	return m, nil
}

// Close releases the model.
func (m *Model) Close() {
	if m == nil || m.ptr == nil {
		return
	}
	C.llama_model_free(m.ptr)
	m.ptr = nil
	m.vocab = nil
	runtime.SetFinalizer(m, nil)
}

// Description returns llama.cpp's model description.
func (m *Model) Description() string {
	if m == nil || m.ptr == nil {
		return ""
	}
	buf := make([]byte, 512)
	n := C.llama_model_desc(m.ptr, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	if n <= 0 {
		return ""
	}
	if int(n) >= len(buf) {
		n = C.int32_t(len(buf) - 1)
	}
	return string(buf[:int(n)])
}

// ContextTrain returns the model's training context length.
func (m *Model) ContextTrain() int {
	if m == nil || m.ptr == nil {
		return 0
	}
	return int(C.llama_model_n_ctx_train(m.ptr))
}

// SlidingWindowAttention returns the model-native sliding-window attention size
// reported by llama.cpp. Zero means the model has no SWA layers.
func (m *Model) SlidingWindowAttention() int {
	if m == nil || m.ptr == nil {
		return 0
	}
	return int(C.llama_model_n_swa(m.ptr))
}

// EmbeddingLength returns the model embedding dimension.
func (m *Model) EmbeddingLength() int {
	if m == nil || m.ptr == nil {
		return 0
	}
	return int(C.llama_model_n_embd(m.ptr))
}

// NumVocab returns the vocabulary size.
func (m *Model) NumVocab() int {
	if m == nil || m.vocab == nil {
		return 0
	}
	return int(C.llama_vocab_n_tokens(m.vocab))
}

// AddBOS reports the model tokenizer's GGUF add_bos_token policy.
func (m *Model) AddBOS() bool {
	if m == nil || m.vocab == nil {
		return false
	}
	return bool(C.llama_vocab_get_add_bos(m.vocab))
}

// TokenIsEOG reports whether token is an end-of-generation token.
func (m *Model) TokenIsEOG(token int) bool {
	if m == nil || m.vocab == nil {
		return false
	}
	return bool(C.llama_vocab_is_eog(m.vocab, C.llama_token(token)))
}

// TokenToPiece converts a token id to rendered text.
func (m *Model) TokenToPiece(token int) string {
	if m == nil || m.vocab == nil {
		return ""
	}
	n := C.int32_t(32)
	buf := make([]byte, int(n))
	got := C.llama_token_to_piece(m.vocab, C.llama_token(token), (*C.char)(unsafe.Pointer(&buf[0])), n, 0, C.bool(true))
	if got < 0 {
		n = -got
		buf = make([]byte, int(n))
		got = C.llama_token_to_piece(m.vocab, C.llama_token(token), (*C.char)(unsafe.Pointer(&buf[0])), n, 0, C.bool(true))
	}
	if got <= 0 {
		return ""
	}
	return strings.TrimRight(string(buf[:int(got)]), "\x00")
}

// Tokenize tokenizes text with the model's active vocab.
func (m *Model) Tokenize(text string, addSpecial bool, parseSpecial bool) ([]int, error) {
	if m == nil || m.ptr == nil || m.vocab == nil {
		return nil, errors.New("llamacppshim: model is closed")
	}
	ctext := C.CString(text)
	defer C.free(unsafe.Pointer(ctext))

	need := C.llama_tokenize(
		m.vocab,
		ctext,
		C.int32_t(len(text)),
		nil,
		0,
		C.bool(addSpecial),
		C.bool(parseSpecial),
	)
	if need == 0 {
		return nil, nil
	}
	if need < 0 {
		need = -need
	}
	toks := make([]C.llama_token, int(need))
	got := C.llama_tokenize(
		m.vocab,
		ctext,
		C.int32_t(len(text)),
		&toks[0],
		need,
		C.bool(addSpecial),
		C.bool(parseSpecial),
	)
	if got < 0 {
		return nil, fmt.Errorf("llamacppshim: tokenize failed")
	}
	out := make([]int, int(got))
	for i := range out {
		out[i] = int(toks[i])
	}
	return out, nil
}

// ApplyChatTemplateCommon renders messages through llama.cpp's common chat
// layer. That path owns Jinja execution, model-specific tool handlers, tool-call
// history, and BOS/EOS normalization.
func (m *Model) ApplyChatTemplateCommon(messagesJSON, toolsJSON string, addAssistant bool) (string, error) {
	result, err := m.ApplyChatTemplateCommonWithOptions(messagesJSON, toolsJSON, ChatTemplateOptions{
		AddAssistant:   addAssistant,
		EnableThinking: true,
	})
	if err != nil {
		return "", err
	}
	return result.Prompt, nil
}

// ChatTemplateOptions controls llama.cpp common chat-template rendering.
type ChatTemplateOptions struct {
	AddAssistant    bool
	ReasoningFormat string
	EnableThinking  bool
	// ReasoningEffort is passed to the template as the reasoning_effort kwarg
	// ("low"/"medium"/"high"). Templates without the variable ignore it; only
	// models whose template consumes it (e.g. gpt-oss harmony) change output.
	ReasoningEffort string
}

// ChatSyntax is the llama.cpp common parser syntax returned by the template
// application path. The format is intentionally opaque to Go; it is passed back
// to llama.cpp's common_chat_parse for model-native output parsing.
type ChatSyntax struct {
	Format           int
	Parser           string
	GenerationPrompt string
}

// ChatTemplateResult is a rendered prompt plus the syntax llama.cpp selected
// from the model's chat template.
type ChatTemplateResult struct {
	Prompt string
	Syntax ChatSyntax
}

// ChatParseResult is llama.cpp common_chat_parse output.
type ChatParseResult struct {
	Content       string
	Thinking      string
	ToolCallsJSON string
}

// ApplyChatTemplateCommonWithOptions renders messages through llama.cpp's common
// chat layer and returns the parser syntax selected for that render.
func (m *Model) ApplyChatTemplateCommonWithOptions(messagesJSON, toolsJSON string, opts ChatTemplateOptions) (ChatTemplateResult, error) {
	if m == nil || m.ptr == nil {
		return ChatTemplateResult{}, errors.New("llamacppshim: model is closed")
	}
	cMsgs := C.CString(messagesJSON)
	defer C.free(unsafe.Pointer(cMsgs))
	cTools := C.CString(toolsJSON)
	defer C.free(unsafe.Pointer(cTools))
	cReasoning := C.CString(opts.ReasoningFormat)
	defer C.free(unsafe.Pointer(cReasoning))

	const errLen = 1024
	errbuf := (*C.char)(C.calloc(1, errLen))
	defer C.free(unsafe.Pointer(errbuf))

	add := C.int(0)
	if opts.AddAssistant {
		add = 1
	}
	enableThinking := C.int(0)
	if opts.EnableThinking {
		enableThinking = 1
	}
	cEffort := C.CString(opts.ReasoningEffort)
	defer C.free(unsafe.Pointer(cEffort))
	result := C.cx_common_chat_apply(m.ptr, cMsgs, cTools, add, cReasoning, enableThinking, cEffort, errbuf, C.size_t(errLen))
	if result.prompt == nil {
		return ChatTemplateResult{}, errors.New("llamacppshim: common chat template: " + C.GoString(errbuf))
	}
	defer C.free(unsafe.Pointer(result.prompt))
	defer C.free(unsafe.Pointer(result.parser))
	defer C.free(unsafe.Pointer(result.generation_prompt))
	return ChatTemplateResult{
		Prompt: C.GoString(result.prompt),
		Syntax: ChatSyntax{
			Format:           int(result.format),
			Parser:           C.GoString(result.parser),
			GenerationPrompt: C.GoString(result.generation_prompt),
		},
	}, nil
}

// ChatTemplateProbe reports what the linked llama.cpp common_chat engine
// detects from a model's own chat template — the capability-truth source for
// tool-call and thinking support. Curated protocol tables are overrides.
type ChatTemplateProbe struct {
	FormatName              string
	ThinkingStartTag        string
	SupportsToolCalls       bool
	SupportsThinking        bool
	SupportsReasoningEffort bool
}

// ProbeChatTemplate inspects the model's chat template via a vocab-only load
// (no tensors). Cost is a metadata+vocab read; callers should cache by digest.
func ProbeChatTemplate(modelPath string) (ChatTemplateProbe, error) {
	initBackend()
	cPath := C.CString(modelPath)
	defer C.free(unsafe.Pointer(cPath))

	const errLen = 1024
	errbuf := (*C.char)(C.calloc(1, errLen))
	defer C.free(unsafe.Pointer(errbuf))

	result := C.cx_probe_chat_template(cPath, errbuf, C.size_t(errLen))
	if result.format_name == nil {
		return ChatTemplateProbe{}, errors.New("llamacppshim: chat template probe: " + C.GoString(errbuf))
	}
	defer C.free(unsafe.Pointer(result.format_name))
	defer C.free(unsafe.Pointer(result.thinking_start_tag))
	return ChatTemplateProbe{
		FormatName:              C.GoString(result.format_name),
		ThinkingStartTag:        C.GoString(result.thinking_start_tag),
		SupportsToolCalls:       result.supports_tool_calls != 0,
		SupportsThinking:        result.supports_thinking != 0,
		SupportsReasoningEffort: result.supports_reasoning_effort != 0,
	}, nil
}

// ParseChatResponse parses generated text through llama.cpp's common response
// parser using syntax returned by ApplyChatTemplateCommonWithOptions.
func ParseChatResponse(input string, partial bool, syntax ChatSyntax, reasoningFormat string, parseToolCalls bool) (ChatParseResult, error) {
	cInput := C.CString(input)
	defer C.free(unsafe.Pointer(cInput))
	cReasoning := C.CString(reasoningFormat)
	defer C.free(unsafe.Pointer(cReasoning))
	cParser := C.CString(syntax.Parser)
	defer C.free(unsafe.Pointer(cParser))
	cGenerationPrompt := C.CString(syntax.GenerationPrompt)
	defer C.free(unsafe.Pointer(cGenerationPrompt))

	const errLen = 1024
	errbuf := (*C.char)(C.calloc(1, errLen))
	defer C.free(unsafe.Pointer(errbuf))

	var cContent *C.char
	var cReasoningOut *C.char
	var cToolCalls *C.char
	isPartial := C.int(0)
	if partial {
		isPartial = 1
	}
	parseTools := C.int(0)
	if parseToolCalls {
		parseTools = 1
	}
	if rc := C.cx_common_chat_parse(
		cInput,
		isPartial,
		C.int(syntax.Format),
		cParser,
		cGenerationPrompt,
		cReasoning,
		parseTools,
		&cContent,
		&cReasoningOut,
		&cToolCalls,
		errbuf,
		C.size_t(errLen),
	); rc != 0 {
		return ChatParseResult{}, errors.New("llamacppshim: common chat parse: " + C.GoString(errbuf))
	}
	defer C.free(unsafe.Pointer(cContent))
	defer C.free(unsafe.Pointer(cReasoningOut))
	defer C.free(unsafe.Pointer(cToolCalls))
	return ChatParseResult{
		Content:       C.GoString(cContent),
		Thinking:      C.GoString(cReasoningOut),
		ToolCallsJSON: C.GoString(cToolCalls),
	}, nil
}

func openAIChatToolSyntax(toolsJSON string) (ChatSyntax, error) {
	cTools := C.CString(toolsJSON)
	defer C.free(unsafe.Pointer(cTools))

	const errLen = 1024
	errbuf := (*C.char)(C.calloc(1, errLen))
	defer C.free(unsafe.Pointer(errbuf))

	result := C.cx_common_chat_syntax_openai_tools(cTools, errbuf, C.size_t(errLen))
	if result.parser == nil {
		return ChatSyntax{}, errors.New("llamacppshim: common chat syntax: " + C.GoString(errbuf))
	}
	defer C.free(unsafe.Pointer(result.parser))
	return ChatSyntax{
		Format: int(result.format),
		Parser: C.GoString(result.parser),
	}, nil
}

// FlashAttnMode selects llama.cpp Flash Attention behavior. The zero value is
// Off, so a context that does not set it keeps FA disabled (the legacy default).
type FlashAttnMode int

const (
	// FlashAttnOff never uses Flash Attention.
	FlashAttnOff FlashAttnMode = iota
	// FlashAttnOn forces Flash Attention on without a support check.
	FlashAttnOn
	// FlashAttnAuto lets llama.cpp enable Flash Attention wherever the device
	// supports it (probed per layer at context build) and fall back to off
	// otherwise. With FA on, the V cache is not transposed.
	FlashAttnAuto
)

// ContextConfig controls direct llama_context construction.
type ContextConfig struct {
	NumCtx       int
	NumBatch     int
	NumSeqMax    int
	NumThreads   int
	FlashAttn    FlashAttnMode
	KVCacheType  string
	Embeddings   bool
	OffloadKQV   bool
	KVUnified    bool
	NoPerf       bool
	PoolingLast  bool
	NonCausalAtt bool
}

// Context wraps a direct llama_context pointer.
type Context struct {
	ptr   *C.struct_llama_context
	model *Model
}

// NewContext creates a direct llama.cpp context for model.
func NewContext(model *Model, cfg ContextConfig) (*Context, error) {
	if model == nil || model.ptr == nil {
		return nil, errors.New("llamacppshim: model is closed")
	}
	params := C.llama_context_default_params()
	if cfg.NumCtx > 0 {
		params.n_ctx = C.uint32_t(cfg.NumCtx)
	}
	if cfg.NumBatch > 0 {
		params.n_batch = C.uint32_t(cfg.NumBatch)
		params.n_ubatch = C.uint32_t(cfg.NumBatch)
	}
	if cfg.NumSeqMax > 0 {
		params.n_seq_max = C.uint32_t(cfg.NumSeqMax)
	} else {
		params.n_seq_max = 1
	}
	if cfg.NumThreads > 0 {
		params.n_threads = C.int32_t(cfg.NumThreads)
		params.n_threads_batch = C.int32_t(cfg.NumThreads)
	}
	switch cfg.FlashAttn {
	case FlashAttnOn:
		params.flash_attn_type = C.LLAMA_FLASH_ATTN_TYPE_ENABLED
	case FlashAttnAuto:
		params.flash_attn_type = C.LLAMA_FLASH_ATTN_TYPE_AUTO
	default:
		params.flash_attn_type = C.LLAMA_FLASH_ATTN_TYPE_DISABLED
	}
	params.type_k = kvCacheType(cfg.KVCacheType)
	params.type_v = kvCacheType(cfg.KVCacheType)
	params.embeddings = C.bool(cfg.Embeddings)
	params.offload_kqv = C.bool(cfg.OffloadKQV)
	params.kv_unified = C.bool(cfg.KVUnified)
	params.no_perf = C.bool(cfg.NoPerf)
	if cfg.PoolingLast {
		params.pooling_type = C.LLAMA_POOLING_TYPE_LAST
	}
	if cfg.NonCausalAtt {
		params.attention_type = C.LLAMA_ATTENTION_TYPE_NON_CAUSAL
	}

	ptr := C.llama_init_from_model(model.ptr, params)
	if ptr == nil {
		return nil, errors.New("llamacppshim: unable to create llama context")
	}
	ctx := &Context{ptr: ptr, model: model}
	runtime.SetFinalizer(ctx, (*Context).Close)
	return ctx, nil
}

func kvCacheType(s string) C.enum_ggml_type {
	switch strings.ToLower(s) {
	case "q8_0":
		return C.GGML_TYPE_Q8_0
	case "q4_0":
		return C.GGML_TYPE_Q4_0
	default:
		return C.GGML_TYPE_F16
	}
}

// Close releases the context.
func (c *Context) Close() {
	if c == nil || c.ptr == nil {
		return
	}
	C.llama_free(c.ptr)
	c.ptr = nil
	runtime.SetFinalizer(c, nil)
}

// Adapter wraps a loaded LoRA adapter. It is owned by the Model it was loaded
// against: llama.cpp frees any still-attached adapters when the model is freed,
// so Free must be called before the owning Model is closed (or not at all).
// Applying an adapter mutates a context, never the base model weights.
type Adapter struct {
	ptr   *C.struct_llama_adapter_lora
	model *Model
}

// LoadAdapter loads a GGUF LoRA adapter for this model. The adapter is not applied
// to any context until Context.SetAdapter is called. Loading reads the adapter's
// GGUF but does not modify the base model weights.
func (m *Model) LoadAdapter(path string) (*Adapter, error) {
	if m == nil || m.ptr == nil {
		return nil, errors.New("llamacppshim: model is closed")
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	ptr := C.llama_adapter_lora_init(m.ptr, cpath)
	if ptr == nil {
		return nil, fmt.Errorf("llamacppshim: failed to load LoRA adapter %q", path)
	}
	return &Adapter{ptr: ptr, model: m}, nil
}

// MetaValue returns a string value from the adapter's GGUF metadata, or "" if the
// key is absent. Used for validation/provenance (e.g. the expected base model).
func (a *Adapter) MetaValue(key string) string {
	if a == nil || a.ptr == nil {
		return ""
	}
	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))
	buf := make([]byte, 256)
	n := C.llama_adapter_meta_val_str(a.ptr, ckey, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	if n < 0 {
		return ""
	}
	if int(n) >= len(buf) {
		n = C.int32_t(len(buf) - 1)
	}
	return string(buf[:int(n)])
}

// Free releases the adapter. Safe to call more than once. Must be called before
// the owning Model is closed; adapters still attached to a freed model are freed
// by llama.cpp, so a later Free would double-free.
func (a *Adapter) Free() {
	if a == nil || a.ptr == nil {
		return
	}
	C.llama_adapter_lora_free(a.ptr)
	a.ptr = nil
}

// SetAdapter applies a loaded LoRA adapter to this context at the given scale.
// This mutates the context only; the base model weights are unchanged.
func (c *Context) SetAdapter(a *Adapter, scale float32) error {
	if c == nil || c.ptr == nil {
		return errors.New("llamacppshim: context is closed")
	}
	if a == nil || a.ptr == nil {
		return errors.New("llamacppshim: adapter is closed")
	}
	adapter := a.ptr
	cscale := C.float(scale)
	if rc := C.llama_set_adapters_lora(c.ptr, &adapter, C.size_t(1), &cscale); rc != 0 {
		return fmt.Errorf("llamacppshim: set LoRA adapter failed (rc=%d)", int(rc))
	}
	return nil
}

// ClearAdapters removes all LoRA adapters from this context (the adapters remain
// loaded and can be re-applied).
func (c *Context) ClearAdapters() {
	if c == nil || c.ptr == nil {
		return
	}
	_ = C.llama_set_adapters_lora(c.ptr, nil, C.size_t(0), nil)
}

// ClearMemory clears llama.cpp memory/KV state.
func (c *Context) ClearMemory(data bool) {
	if c == nil || c.ptr == nil {
		return
	}
	C.llama_memory_clear(C.llama_get_memory(c.ptr), C.bool(data))
}

// MemorySeqRemove removes KV entries for seqID in [p0, p1).
func (c *Context) MemorySeqRemove(seqID, p0, p1 int) bool {
	if c == nil || c.ptr == nil {
		return false
	}
	return bool(C.llama_memory_seq_rm(C.llama_get_memory(c.ptr), C.llama_seq_id(seqID), C.llama_pos(p0), C.llama_pos(p1)))
}

// MemorySeqCopy copies KV state from srcSeqID to dstSeqID.
func (c *Context) MemorySeqCopy(srcSeqID, dstSeqID, p0, p1 int) bool {
	if c == nil || c.ptr == nil {
		return false
	}
	C.llama_memory_seq_cp(C.llama_get_memory(c.ptr), C.llama_seq_id(srcSeqID), C.llama_seq_id(dstSeqID), C.llama_pos(p0), C.llama_pos(p1))
	return true
}

// MemorySeqAdd shifts positions for seqID.
func (c *Context) MemorySeqAdd(seqID, p0, p1, delta int) bool {
	if c == nil || c.ptr == nil {
		return false
	}
	C.llama_memory_seq_add(C.llama_get_memory(c.ptr), C.llama_seq_id(seqID), C.llama_pos(p0), C.llama_pos(p1), C.llama_pos(delta))
	return true
}

// DecodeStatus preserves llama_decode's exact status class.
type DecodeStatus string

const (
	DecodeOK       DecodeStatus = "ok"
	DecodeNoKVSlot DecodeStatus = "no_kv_slot"
	DecodeAborted  DecodeStatus = "aborted_partial"
	DecodeInvalid  DecodeStatus = "invalid_batch"
	DecodeFatal    DecodeStatus = "fatal"
)

// DecodeResult reports llama_decode status without collapsing warning/fatal
// cases into a generic error.
type DecodeResult struct {
	Status DecodeStatus
	Code   int
	Err    error
}

// Decode runs one batch through llama_decode.
func (c *Context) Decode(batch *Batch) DecodeResult {
	if c == nil || c.ptr == nil {
		return DecodeResult{Status: DecodeFatal, Code: -2, Err: errors.New("llamacppshim: context is closed")}
	}
	if batch == nil {
		return DecodeResult{Status: DecodeInvalid, Code: -1, Err: errors.New("llamacppshim: nil batch")}
	}
	code := int(C.llama_decode(c.ptr, batch.c))
	switch {
	case code == 0:
		return DecodeResult{Status: DecodeOK, Code: code}
	case code == 1:
		return DecodeResult{Status: DecodeNoKVSlot, Code: code, Err: errors.New("llamacppshim: no kv slot")}
	case code == 2:
		return DecodeResult{Status: DecodeAborted, Code: code, Err: errors.New("llamacppshim: decode aborted with partial memory")}
	case code == -1:
		return DecodeResult{Status: DecodeInvalid, Code: code, Err: errors.New("llamacppshim: invalid decode batch")}
	default:
		return DecodeResult{Status: DecodeFatal, Code: code, Err: fmt.Errorf("llamacppshim: fatal decode code %d", code)}
	}
}

// GetEmbeddingsSeq returns pooled embeddings for seqID, if available.
func (c *Context) GetEmbeddingsSeq(seqID int) []float32 {
	if c == nil || c.ptr == nil || c.model == nil {
		return nil
	}
	ptr := C.llama_get_embeddings_seq(c.ptr, C.llama_seq_id(seqID))
	if ptr == nil {
		return nil
	}
	n := c.model.EmbeddingLength()
	out := make([]float32, n)
	copy(out, unsafe.Slice((*float32)(unsafe.Pointer(ptr)), n))
	return out
}

// GetEmbeddingsIth returns token embeddings for output row i, if available.
func (c *Context) GetEmbeddingsIth(i int) []float32 {
	if c == nil || c.ptr == nil || c.model == nil {
		return nil
	}
	ptr := C.llama_get_embeddings_ith(c.ptr, C.int32_t(i))
	if ptr == nil {
		return nil
	}
	n := c.model.EmbeddingLength()
	out := make([]float32, n)
	copy(out, unsafe.Slice((*float32)(unsafe.Pointer(ptr)), n))
	return out
}

// Batch wraps a llama_batch allocated by llama.cpp.
type Batch struct {
	c         C.struct_llama_batch
	batchSize int
	maxSeq    int
	embedSize int
}

// NewBatch creates a llama batch.
func NewBatch(batchSize, maxSeq, embedSize int) (*Batch, error) {
	if batchSize <= 0 {
		return nil, errors.New("llamacppshim: batch size must be positive")
	}
	if maxSeq <= 0 {
		maxSeq = 1
	}
	b := &Batch{
		c:         C.llama_batch_init(C.int32_t(batchSize*maxSeq), C.int32_t(embedSize), C.int32_t(maxSeq)),
		batchSize: batchSize,
		maxSeq:    maxSeq,
		embedSize: embedSize,
	}
	if (embedSize == 0 && b.c.token == nil) || b.c.pos == nil || b.c.n_seq_id == nil || b.c.seq_id == nil || b.c.logits == nil {
		C.llama_batch_free(b.c)
		return nil, errors.New("llamacppshim: unable to allocate batch")
	}
	runtime.SetFinalizer(b, (*Batch).Free)
	return b, nil
}

func (b *Batch) allocSize() int { return b.batchSize * b.maxSeq }

// Clear resets the batch to zero tokens.
func (b *Batch) Clear() {
	if b != nil {
		b.c.n_tokens = 0
	}
}

// Add appends a token or embedding row to the batch.
func (b *Batch) Add(token int, embed []float32, pos int, logits bool, seqIDs ...int) error {
	if b == nil {
		return errors.New("llamacppshim: nil batch")
	}
	i := int(b.c.n_tokens)
	if i >= b.allocSize() {
		return errors.New("llamacppshim: batch is full")
	}
	if b.embedSize == 0 {
		unsafe.Slice(b.c.token, b.allocSize())[i] = C.llama_token(token)
	} else {
		if len(embed) != b.embedSize {
			return fmt.Errorf("llamacppshim: embedding row has %d values, want %d", len(embed), b.embedSize)
		}
		copy(unsafe.Slice((*float32)(b.c.embd), b.allocSize()*b.embedSize)[i*b.embedSize:], embed)
	}
	unsafe.Slice(b.c.pos, b.allocSize())[i] = C.llama_pos(pos)
	unsafe.Slice(b.c.n_seq_id, b.allocSize())[i] = C.int32_t(len(seqIDs))
	for j, seqID := range seqIDs {
		unsafe.Slice(unsafe.Slice(b.c.seq_id, b.allocSize())[i], len(seqIDs))[j] = C.llama_seq_id(seqID)
	}
	if logits {
		unsafe.Slice(b.c.logits, b.allocSize())[i] = 1
	} else {
		unsafe.Slice(b.c.logits, b.allocSize())[i] = 0
	}
	b.c.n_tokens++
	return nil
}

// Free releases the llama batch.
func (b *Batch) Free() {
	if b == nil || b.batchSize == 0 {
		return
	}
	C.llama_batch_free(b.c)
	b.batchSize = 0
	runtime.SetFinalizer(b, nil)
}

// SamplingParams controls the small direct sampler chain used by modeld.
type SamplingParams struct {
	TopK        int
	TopP        float32
	MinP        float32
	Temperature float32
	Seed        uint32
	// GrammarGBNF constrains sampling to a GBNF grammar rooted at "root".
	// Requires the model-aware constructor, because the grammar sampler is
	// built against the model vocabulary.
	GrammarGBNF string
}

// SamplingContext wraps a llama_sampler chain.
type SamplingContext struct {
	ptr *C.struct_llama_sampler
}

// NewSamplingContext creates a minimal sampler chain.
func NewSamplingContext(params SamplingParams) (*SamplingContext, error) {
	return NewSamplingContextForModel(nil, params)
}

// JSONSchemaToGrammar converts a JSON schema document into a GBNF grammar
// rooted at "root", via llama.cpp common's json_schema_to_grammar — the same
// converter llama-server uses for its structured output.
func JSONSchemaToGrammar(schemaJSON string) (string, error) {
	cSchema := C.CString(schemaJSON)
	defer C.free(unsafe.Pointer(cSchema))
	var out *C.char
	rc := C.cx_json_schema_to_grammar(cSchema, &out)
	if out == nil {
		return "", errors.New("llamacppshim: json schema to grammar returned nothing")
	}
	defer C.free(unsafe.Pointer(out))
	if rc != 0 {
		return "", fmt.Errorf("llamacppshim: %s", C.GoString(out))
	}
	return C.GoString(out), nil
}

// NewSamplingContextForModel creates a sampler chain, optionally constrained
// by params.GrammarGBNF. The grammar sampler masks non-conforming token
// logits before the truncation/selection samplers run, so it sits first in
// the chain; Accept propagates sampled tokens to it like any chain member.
func NewSamplingContextForModel(m *Model, params SamplingParams) (*SamplingContext, error) {
	cparams := C.llama_sampler_chain_default_params()
	chain := C.llama_sampler_chain_init(cparams)
	if chain == nil {
		return nil, errors.New("llamacppshim: create sampler chain")
	}
	if params.GrammarGBNF != "" {
		if m == nil || m.ptr == nil || m.vocab == nil {
			C.llama_sampler_free(chain)
			return nil, errors.New("llamacppshim: grammar sampling requires an open model vocabulary")
		}
		cGrammar := C.CString(params.GrammarGBNF)
		cRoot := C.CString("root")
		grammar := C.llama_sampler_init_grammar(m.vocab, cGrammar, cRoot)
		C.free(unsafe.Pointer(cGrammar))
		C.free(unsafe.Pointer(cRoot))
		if grammar == nil {
			C.llama_sampler_free(chain)
			return nil, errors.New("llamacppshim: create grammar sampler (invalid GBNF?)")
		}
		C.llama_sampler_chain_add(chain, grammar)
	}
	if params.TopK > 0 {
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_top_k(C.int32_t(params.TopK)))
	}
	if params.TopP > 0 && params.TopP < 1 {
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_top_p(C.float(params.TopP), 1))
	}
	if params.MinP > 0 {
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_min_p(C.float(params.MinP), 1))
	}
	if params.Temperature <= 0 {
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_greedy())
	} else {
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_temp(C.float(params.Temperature)))
		C.llama_sampler_chain_add(chain, C.llama_sampler_init_dist(C.uint32_t(params.Seed)))
	}
	s := &SamplingContext{ptr: chain}
	runtime.SetFinalizer(s, (*SamplingContext).Free)
	return s, nil
}

// Sample samples a token from context logits.
func (s *SamplingContext) Sample(ctx *Context, idx int) int {
	if s == nil || s.ptr == nil || ctx == nil || ctx.ptr == nil {
		return -1
	}
	return int(C.llama_sampler_sample(s.ptr, ctx.ptr, C.int32_t(idx)))
}

// Accept accepts a sampled token into the sampler history.
func (s *SamplingContext) Accept(id int) {
	if s == nil || s.ptr == nil {
		return
	}
	C.llama_sampler_accept(s.ptr, C.llama_token(id))
}

// Free releases the sampler chain.
func (s *SamplingContext) Free() {
	if s == nil || s.ptr == nil {
		return
	}
	C.llama_sampler_free(s.ptr)
	s.ptr = nil
	runtime.SetFinalizer(s, nil)
}

// StateGetData returns full context state bytes, including output logits and
// memory. Use this for session snapshots that must resume the next sample.
func (c *Context) StateGetData() ([]byte, error) {
	if c == nil || c.ptr == nil {
		return nil, errors.New("llamacppshim: context is closed")
	}
	n := C.llama_state_get_size(c.ptr)
	if n == 0 {
		return nil, errors.New("llamacppshim: empty context state")
	}
	buf := make([]byte, int(n))
	got := C.llama_state_get_data(c.ptr, (*C.uint8_t)(unsafe.Pointer(&buf[0])), n)
	if got != n {
		return nil, fmt.Errorf("llamacppshim: copied %d context state bytes, want %d", uint64(got), uint64(n))
	}
	return buf, nil
}

// StateSetData restores bytes previously returned by StateGetData.
func (c *Context) StateSetData(data []byte) error {
	if c == nil || c.ptr == nil {
		return errors.New("llamacppshim: context is closed")
	}
	if len(data) == 0 {
		return errors.New("llamacppshim: empty context state")
	}
	got := C.llama_state_set_data(c.ptr, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)))
	if got != C.size_t(len(data)) {
		return fmt.Errorf("llamacppshim: loaded %d context state bytes, want %d", uint64(got), len(data))
	}
	return nil
}

// StateSeqGetData returns direct sequence memory state bytes.
func (c *Context) StateSeqGetData(seqID int) ([]byte, error) {
	if c == nil || c.ptr == nil {
		return nil, errors.New("llamacppshim: context is closed")
	}
	n := C.llama_state_seq_get_size(c.ptr, C.llama_seq_id(seqID))
	if n == 0 {
		return nil, errors.New("llamacppshim: empty sequence state")
	}
	buf := make([]byte, int(n))
	got := C.llama_state_seq_get_data(c.ptr, (*C.uint8_t)(unsafe.Pointer(&buf[0])), n, C.llama_seq_id(seqID))
	if got != n {
		return nil, fmt.Errorf("llamacppshim: copied %d bytes, want %d", uint64(got), uint64(n))
	}
	return buf, nil
}

// StateSeqSetData restores direct sequence memory state bytes.
func (c *Context) StateSeqSetData(seqID int, data []byte) error {
	if c == nil || c.ptr == nil {
		return errors.New("llamacppshim: context is closed")
	}
	if len(data) == 0 {
		return errors.New("llamacppshim: empty state")
	}
	got := C.llama_state_seq_set_data(c.ptr, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), C.llama_seq_id(seqID))
	if got == 0 {
		return errors.New("llamacppshim: seq state load failed")
	}
	return nil
}
