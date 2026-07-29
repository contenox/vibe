//go:build openvino && openvino_genai

package ovsession

/*
#cgo CXXFLAGS: -std=c++17
#include <stdlib.h>
#include "genai.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/contenox/contenox/internal/modeld/internal/sessionkit"
)

// GenAIAvailable reports whether the OpenVINO GenAI session backend was built.
const GenAIAvailable = true

const genAIErrLen = 4 * 1024

// GenAIConfig controls construction of an OpenVINO GenAI session.
type GenAIConfig struct {
	Device                      string
	KVCachePrecision            string
	CacheSize                   int
	CacheSizeExplicit           bool
	DynamicSplitFuse            *bool
	EnablePrefixCaching         *bool
	UseSparseAttention          *bool
	NumLastDenseTokensInPrefill int
	XAttentionThreshold         float32
	XAttentionBlockSize         int
	XAttentionStride            int
	// UseCacheEviction enables OpenVINO's native sink+recent+evictable KV cache
	// eviction (the declarative form of the residency policy). Sizes are in
	// tokens; CacheEvictMaxSize must exceed start + recent.
	UseCacheEviction     *bool
	CacheEvictStartSize  int
	CacheEvictRecentSize int
	CacheEvictMaxSize    int
	// LoRAAdapters are dynamic LoRA adapters registered on the pipeline at
	// construction (MODE_DYNAMIC) and activated in the default generation config.
	// Empty = base model. OpenVINO adapters are safetensors files, not GGUF.
	LoRAAdapters []GenAILoRAAdapter
}

// GenAILoRAAdapter is one OpenVINO LoRA adapter to apply to a session: a
// safetensors file plus its effective scale (alpha). OpenVINO folds LoRA rank
// normalization and any user weight into this single alpha.
type GenAILoRAAdapter struct {
	Path  string
	Alpha float32
}

// GenerateOptions controls a single GenAI generation call.
type GenerateOptions struct {
	MaxNewTokens     int
	Temperature      *float64
	TopP             *float64
	TopK             *int
	Seed             *int
	StructuredOutput StructuredOutput
	ParserProtocols  []string
}

// StructuredOutput selects an OpenVINO structured-output primitive plus its
// payload for this generation call.
type StructuredOutput struct {
	Protocol string
	Payload  string
}

// PipelineMetrics mirrors the OpenVINO GenAI PipelineMetrics fields used by
// the local runtime.
type PipelineMetrics struct {
	Requests          uint64
	ScheduledRequests uint64
	CacheUsage        float32
	MaxCacheUsage     float32
	AvgCacheUsage     float32
	InferenceDuration float32
	CacheSizeInBytes  uint64
}

// DeviceInfo describes one OpenVINO device and any memory telemetry exposed by
// that plugin. CPU devices report zero memory here; modeld uses system RAM for
// CPU capacity planning.
type DeviceInfo struct {
	Index             int
	Name              string
	Description       string
	Type              string
	MemoryFree        uint64
	MemoryTotal       uint64
	MemoryFreeKnown   bool
	MemoryTotalKnown  bool
	SharedWithDisplay bool
}

// RuntimeInfo describes the linked OpenVINO runtime and available devices.
type RuntimeInfo struct {
	RuntimeName        string
	RuntimeDigest      string
	RuntimeSystemInfo  string
	SupportsGPUOffload bool
	Devices            []DeviceInfo
}

// ModelKVProfile is the model architecture data needed to size KV cache. It is
// read through the native OpenVINO shim so service capacity planning uses the
// same model metadata boundary as the backend.
type ModelKVProfile struct {
	MaxPositionEmbeddings int
	NumHiddenLayers       int
	NumKeyValueHeads      int
	NumAttentionHeads     int
	HiddenSize            int
	HeadDim               int
	SlidingWindow         int
	GlobalLayers          int
	WindowedLayers        int
}

// ChatTemplateProbe reports what OpenVINO GenAI's tokenizer/chat-template
// renderer actually does for this model directory.
type ChatTemplateProbe struct {
	FormatName              string
	ThinkingStartTag        string
	SupportsToolCalls       bool
	SupportsThinking        bool
	SupportsReasoningEffort bool
}

// GenAIResult is the generated text plus the pipeline metrics observed for the
// request.
type GenAIResult struct {
	Text       string
	ParsedJSON string
	Metrics    PipelineMetrics
}

// StreamChunk carries a decoded text delta or a terminal stream error.
type StreamChunk struct {
	Text     string
	Thinking string
	Error    error
}

// GenAISession wraps a single ContinuousBatchingPipeline.
type GenAISession struct {
	mu  sync.Mutex
	ptr *C.cx_genai_session
}

// Runtime reports the linked OpenVINO runtime identity and available devices.
func Runtime() (RuntimeInfo, error) {
	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return RuntimeInfo{}, errors.New("allocate OpenVINO runtime info error buffer")
	}
	defer C.free(errbuf)

	var out C.cx_ov_runtime_info
	if rc := C.cx_ov_runtime_info_get(&out, (*C.char)(errbuf), C.size_t(genAIErrLen)); rc != 0 {
		return RuntimeInfo{}, fmt.Errorf("openvino runtime info: %s", C.GoString((*C.char)(errbuf)))
	}

	info := RuntimeInfo{
		RuntimeName:        cString(out.runtime_name[:]),
		RuntimeDigest:      cString(out.runtime_digest[:]),
		RuntimeSystemInfo:  cString(out.system_info[:]),
		SupportsGPUOffload: out.supports_gpu_offload != 0,
	}
	count := int(out.device_count)
	if count > len(out.devices) {
		count = len(out.devices)
	}
	for i := 0; i < count; i++ {
		info.Devices = append(info.Devices, deviceInfoFromC(out.devices[i]))
	}
	return info, nil
}

// Device returns telemetry for the selected OpenVINO device name, resolving
// aliases such as "GPU" to the first available OpenVINO GPU device.
func Device(device string) (DeviceInfo, error) {
	cDev := C.CString(device)
	defer C.free(unsafe.Pointer(cDev))
	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return DeviceInfo{}, errors.New("allocate OpenVINO device info error buffer")
	}
	defer C.free(errbuf)

	var out C.cx_ov_device_info
	if rc := C.cx_ov_device_info_get(cDev, &out, (*C.char)(errbuf), C.size_t(genAIErrLen)); rc != 0 {
		return DeviceInfo{}, fmt.Errorf("openvino device info: %s", C.GoString((*C.char)(errbuf)))
	}
	return deviceInfoFromC(out), nil
}

// InspectModelKVProfile returns model KV sizing metadata from the native
// OpenVINO shim without opening a GenAI pipeline.
func InspectModelKVProfile(modelDir string) (ModelKVProfile, error) {
	cDir := C.CString(modelDir)
	defer C.free(unsafe.Pointer(cDir))
	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return ModelKVProfile{}, errors.New("allocate OpenVINO model KV profile error buffer")
	}
	defer C.free(errbuf)

	var out C.cx_ov_model_kv_profile
	if rc := C.cx_ov_model_kv_profile_get(cDir, &out, (*C.char)(errbuf), C.size_t(genAIErrLen)); rc != 0 {
		return ModelKVProfile{}, fmt.Errorf("openvino model KV profile: %s", C.GoString((*C.char)(errbuf)))
	}
	return ModelKVProfile{
		MaxPositionEmbeddings: int(out.max_position_embeddings),
		NumHiddenLayers:       int(out.num_hidden_layers),
		NumKeyValueHeads:      int(out.num_key_value_heads),
		NumAttentionHeads:     int(out.num_attention_heads),
		HiddenSize:            int(out.hidden_size),
		HeadDim:               int(out.head_dim),
		SlidingWindow:         int(out.sliding_window),
		GlobalLayers:          int(out.global_layers),
		WindowedLayers:        int(out.windowed_layers),
	}, nil
}

// ProbeChatTemplate inspects the model's own OpenVINO GenAI chat template
// through the native tokenizer renderer, without opening the LLM pipeline.
func ProbeChatTemplate(modelDir string) (ChatTemplateProbe, error) {
	cDir := C.CString(modelDir)
	defer C.free(unsafe.Pointer(cDir))
	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return ChatTemplateProbe{}, errors.New("allocate OpenVINO chat template probe error buffer")
	}
	defer C.free(errbuf)

	var out C.cx_ov_chat_template_probe
	if rc := C.cx_ov_chat_template_probe_get(cDir, &out, (*C.char)(errbuf), C.size_t(genAIErrLen)); rc != 0 {
		return ChatTemplateProbe{}, fmt.Errorf("openvino chat template probe: %s", C.GoString((*C.char)(errbuf)))
	}
	return ChatTemplateProbe{
		FormatName:              cString(out.format_name[:]),
		ThinkingStartTag:        cString(out.thinking_start_tag[:]),
		SupportsToolCalls:       out.supports_tool_calls != 0,
		SupportsThinking:        out.supports_thinking != 0,
		SupportsReasoningEffort: out.supports_reasoning_effort != 0,
	}, nil
}

func deviceInfoFromC(d C.cx_ov_device_info) DeviceInfo {
	return DeviceInfo{
		Index:             int(d.index),
		Name:              cString(d.name[:]),
		Description:       cString(d.description[:]),
		Type:              cString(d._type[:]),
		MemoryFree:        uint64(d.memory_free),
		MemoryTotal:       uint64(d.memory_total),
		MemoryFreeKnown:   d.memory_free_known != 0,
		MemoryTotalKnown:  d.memory_total_known != 0,
		SharedWithDisplay: d.shared_with_display != 0,
	}
}

func cString(buf []C.char) string {
	if len(buf) == 0 {
		return ""
	}
	return C.GoString((*C.char)(unsafe.Pointer(&buf[0])))
}

func cAllocatedText(p *C.char, n C.size_t) string {
	if p == nil || n == 0 {
		return ""
	}
	return strings.Clone(unsafe.String((*byte)(unsafe.Pointer(p)), int(n)))
}

func freeCData(p unsafe.Pointer) {
	if p != nil {
		C.cx_genai_data_free(p)
	}
}

// NewGenAI creates an OpenVINO GenAI ContinuousBatchingPipeline session.
func NewGenAI(modelDir string, cfg GenAIConfig) (*GenAISession, error) {
	if modelDir == "" {
		return nil, errors.New("openvino GenAI model directory is required")
	}
	device := cfg.Device
	if device == "" {
		device = "CPU"
	}
	cfg = normalizeGenAIConfig(cfg)

	cDir := C.CString(modelDir)
	cDev := C.CString(device)
	cKVPrecision := C.CString(cfg.KVCachePrecision)
	defer C.free(unsafe.Pointer(cDir))
	defer C.free(unsafe.Pointer(cDev))
	defer C.free(unsafe.Pointer(cKVPrecision))

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return nil, errors.New("allocate OpenVINO GenAI error buffer")
	}
	defer C.free(errbuf)

	// Marshal dynamic LoRA adapters into a C array. The array and its path strings
	// are allocated in C memory (not a Go slice) so the cConfig we hand to C holds
	// no Go pointers — cgo forbids passing Go memory that itself contains Go
	// pointers. The C side copies the specs during cx_genai_session_new, so freeing
	// after the call is safe.
	var cAdapters *C.cx_genai_lora_adapter
	if n := len(cfg.LoRAAdapters); n > 0 {
		cAdapters = (*C.cx_genai_lora_adapter)(C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(C.cx_genai_lora_adapter{}))))
		defer C.free(unsafe.Pointer(cAdapters))
		arr := unsafe.Slice(cAdapters, n)
		for i, a := range cfg.LoRAAdapters {
			cPath := C.CString(a.Path)
			defer C.free(unsafe.Pointer(cPath))
			arr[i] = C.cx_genai_lora_adapter{path: cPath, alpha: C.float(a.Alpha)}
		}
	}

	cConfig := C.cx_genai_session_config{
		kv_cache_precision:               cKVPrecision,
		cache_size:                       C.size_t(cfg.CacheSize),
		dynamic_split_fuse:               cbool(boolValue(cfg.DynamicSplitFuse, true)),
		enable_prefix_caching:            cbool(boolValue(cfg.EnablePrefixCaching, true)),
		use_sparse_attention:             cbool(boolValue(cfg.UseSparseAttention, true)),
		num_last_dense_tokens_in_prefill: C.size_t(cfg.NumLastDenseTokensInPrefill),
		xattention_threshold:             C.float(cfg.XAttentionThreshold),
		xattention_block_size:            C.size_t(cfg.XAttentionBlockSize),
		xattention_stride:                C.size_t(cfg.XAttentionStride),
		use_cache_eviction:               cbool(boolValue(cfg.UseCacheEviction, false)),
		cache_evict_start_size:           C.size_t(max(cfg.CacheEvictStartSize, 0)),
		cache_evict_recent_size:          C.size_t(max(cfg.CacheEvictRecentSize, 0)),
		cache_evict_max_size:             C.size_t(max(cfg.CacheEvictMaxSize, 0)),
		lora_adapters:                    cAdapters,
		lora_adapter_count:               C.size_t(len(cfg.LoRAAdapters)),
	}

	ptr := C.cx_genai_session_new(cDir, cDev, &cConfig, (*C.char)(errbuf), C.size_t(genAIErrLen))
	if ptr == nil {
		return nil, fmt.Errorf("openvino GenAI session new: %s", C.GoString((*C.char)(errbuf)))
	}

	s := &GenAISession{ptr: ptr}
	runtime.SetFinalizer(s, (*GenAISession).mustClose)
	return s, nil
}

// Generate runs one prompt through the GenAI session.
func (s *GenAISession) Generate(ctx context.Context, prompt string, opts GenerateOptions) (GenAIResult, error) {
	if err := ctx.Err(); err != nil {
		return GenAIResult{}, err
	}
	if prompt == "" {
		return GenAIResult{}, errors.New("openvino GenAI prompt is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return GenAIResult{}, errors.New("openvino GenAI session is closed")
	}

	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))
	cStructuredProtocol := C.CString(opts.StructuredOutput.Protocol)
	cStructuredPayload := C.CString(opts.StructuredOutput.Payload)
	cParserProtocols := C.CString(strings.Join(opts.ParserProtocols, "\n"))
	defer C.free(unsafe.Pointer(cStructuredProtocol))
	defer C.free(unsafe.Pointer(cStructuredPayload))
	defer C.free(unsafe.Pointer(cParserProtocols))

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return GenAIResult{}, errors.New("allocate OpenVINO GenAI error buffer")
	}
	defer C.free(errbuf)

	var cmetrics C.cx_genai_metrics
	var temp C.float
	var useTemp C.int
	if opts.Temperature != nil {
		temp = C.float(*opts.Temperature)
		useTemp = 1
	}
	var topP C.float
	var useTopP C.int
	if opts.TopP != nil {
		topP = C.float(*opts.TopP)
		useTopP = 1
	}
	var topK C.size_t
	var useTopK C.int
	if opts.TopK != nil && *opts.TopK > 0 {
		topK = C.size_t(*opts.TopK)
		useTopK = 1
	}
	var seed C.size_t
	var useSeed C.int
	if opts.Seed != nil && *opts.Seed >= 0 {
		seed = C.size_t(*opts.Seed)
		useSeed = 1
	}

	var out *C.char
	var outLen C.size_t
	var parsed *C.char
	var parsedLen C.size_t
	done := make(chan struct{})
	if ctx.Done() != nil {
		ptr := s.ptr
		go func() {
			select {
			case <-ctx.Done():
				C.cx_genai_session_cancel(ptr)
			case <-done:
			}
		}()
	}
	// Re-check after acquiring the lock and arming the cancel watcher: a context
	// canceled during that setup window would otherwise reach the native
	// generate call, where a per-generation cancel-flag reset can race the
	// watcher's cancel. Skipping the native call when already canceled closes
	// that window for the common pre-canceled path.
	if err := ctx.Err(); err != nil {
		close(done)
		return GenAIResult{}, err
	}
	rc := C.cx_genai_generate(
		s.ptr,
		cPrompt,
		C.size_t(max(opts.MaxNewTokens, 0)),
		temp,
		useTemp,
		topP,
		useTopP,
		topK,
		useTopK,
		seed,
		useSeed,
		cStructuredProtocol,
		cStructuredPayload,
		cParserProtocols,
		&out,
		&outLen,
		&parsed,
		&parsedLen,
		&cmetrics,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	defer freeCData(unsafe.Pointer(out))
	defer freeCData(unsafe.Pointer(parsed))
	close(done)
	if rc != 0 {
		if rc == 3 {
			if err := ctx.Err(); err != nil {
				return GenAIResult{}, err
			}
			return GenAIResult{}, errors.New("openvino GenAI generation canceled")
		}
		return GenAIResult{}, fmt.Errorf("openvino GenAI generate: %s", C.GoString((*C.char)(errbuf)))
	}
	if err := ctx.Err(); err != nil {
		return GenAIResult{}, err
	}

	return GenAIResult{
		Text:       cAllocatedText(out, outLen),
		ParsedJSON: cAllocatedText(parsed, parsedLen),
		Metrics:    pipelineMetricsFromC(cmetrics),
	}, nil
}

// PrefillTokens submits an already-tokenized prompt with zero generation so
// OpenVINO GenAI materializes prefix-cache KV for subsequent export/reuse.
func (s *GenAISession) PrefillTokens(ctx context.Context, tokens []int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(tokens) == 0 {
		return errors.New("openvino GenAI token prompt is empty")
	}
	cTokens := make([]C.int64_t, len(tokens))
	for i, tok := range tokens {
		cTokens[i] = C.int64_t(tok)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return errors.New("openvino GenAI session is closed")
	}

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return errors.New("allocate OpenVINO GenAI token prefill error buffer")
	}
	defer C.free(errbuf)

	var cmetrics C.cx_genai_metrics
	done := make(chan struct{})
	if ctx.Done() != nil {
		ptr := s.ptr
		go func() {
			select {
			case <-ctx.Done():
				C.cx_genai_session_cancel(ptr)
			case <-done:
			}
		}()
	}
	rc := C.cx_genai_prefill_tokens(
		s.ptr,
		(*C.int64_t)(unsafe.Pointer(&cTokens[0])),
		C.size_t(len(cTokens)),
		&cmetrics,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	close(done)
	if rc != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("openvino GenAI token prefill: %s", C.GoString((*C.char)(errbuf)))
	}
	return ctx.Err()
}

// GenerateTokens runs one already-tokenized prompt through the GenAI session.
func (s *GenAISession) GenerateTokens(ctx context.Context, tokens []int, opts GenerateOptions) (GenAIResult, error) {
	if err := ctx.Err(); err != nil {
		return GenAIResult{}, err
	}
	if len(tokens) == 0 {
		return GenAIResult{}, errors.New("openvino GenAI token prompt is empty")
	}
	cTokens := make([]C.int64_t, len(tokens))
	for i, tok := range tokens {
		cTokens[i] = C.int64_t(tok)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return GenAIResult{}, errors.New("openvino GenAI session is closed")
	}

	cStructuredProtocol := C.CString(opts.StructuredOutput.Protocol)
	cStructuredPayload := C.CString(opts.StructuredOutput.Payload)
	cParserProtocols := C.CString(strings.Join(opts.ParserProtocols, "\n"))
	defer C.free(unsafe.Pointer(cStructuredProtocol))
	defer C.free(unsafe.Pointer(cStructuredPayload))
	defer C.free(unsafe.Pointer(cParserProtocols))

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return GenAIResult{}, errors.New("allocate OpenVINO GenAI error buffer")
	}
	defer C.free(errbuf)

	var cmetrics C.cx_genai_metrics
	var temp C.float
	var useTemp C.int
	if opts.Temperature != nil {
		temp = C.float(*opts.Temperature)
		useTemp = 1
	}
	var topP C.float
	var useTopP C.int
	if opts.TopP != nil {
		topP = C.float(*opts.TopP)
		useTopP = 1
	}
	var topK C.size_t
	var useTopK C.int
	if opts.TopK != nil && *opts.TopK > 0 {
		topK = C.size_t(*opts.TopK)
		useTopK = 1
	}
	var seed C.size_t
	var useSeed C.int
	if opts.Seed != nil && *opts.Seed >= 0 {
		seed = C.size_t(*opts.Seed)
		useSeed = 1
	}

	var out *C.char
	var outLen C.size_t
	var parsed *C.char
	var parsedLen C.size_t
	done := make(chan struct{})
	if ctx.Done() != nil {
		ptr := s.ptr
		go func() {
			select {
			case <-ctx.Done():
				C.cx_genai_session_cancel(ptr)
			case <-done:
			}
		}()
	}
	rc := C.cx_genai_generate_tokens(
		s.ptr,
		(*C.int64_t)(unsafe.Pointer(&cTokens[0])),
		C.size_t(len(cTokens)),
		C.size_t(max(opts.MaxNewTokens, 0)),
		temp,
		useTemp,
		topP,
		useTopP,
		topK,
		useTopK,
		seed,
		useSeed,
		cStructuredProtocol,
		cStructuredPayload,
		cParserProtocols,
		&out,
		&outLen,
		&parsed,
		&parsedLen,
		&cmetrics,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	defer freeCData(unsafe.Pointer(out))
	defer freeCData(unsafe.Pointer(parsed))
	close(done)
	if rc != 0 {
		if rc == 3 {
			if err := ctx.Err(); err != nil {
				return GenAIResult{}, err
			}
			return GenAIResult{}, errors.New("openvino GenAI generation canceled")
		}
		return GenAIResult{}, fmt.Errorf("openvino GenAI generate tokens: %s", C.GoString((*C.char)(errbuf)))
	}
	if err := ctx.Err(); err != nil {
		return GenAIResult{}, err
	}

	return GenAIResult{
		Text:       cAllocatedText(out, outLen),
		ParsedJSON: cAllocatedText(parsed, parsedLen),
		Metrics:    pipelineMetricsFromC(cmetrics),
	}, nil
}

func pipelineMetricsFromC(cmetrics C.cx_genai_metrics) PipelineMetrics {
	return PipelineMetrics{
		Requests:          uint64(cmetrics.requests),
		ScheduledRequests: uint64(cmetrics.scheduled_requests),
		CacheUsage:        float32(cmetrics.cache_usage),
		MaxCacheUsage:     float32(cmetrics.max_cache_usage),
		AvgCacheUsage:     float32(cmetrics.avg_cache_usage),
		InferenceDuration: float32(cmetrics.inference_duration),
		CacheSizeInBytes:  uint64(cmetrics.cache_size_in_bytes),
	}
}

func sendTerminalStreamError(ctx context.Context, ch chan<- StreamChunk, err error) {
	if err == nil {
		return
	}
	chunk := StreamChunk{Error: err}
	if ctx.Err() != nil {
		sessionkit.TrySend(ch, chunk)
		return
	}
	_ = sessionkit.Send(ctx, ch, chunk)
}

// Stream runs one prompt and returns decoded text deltas as GenAI produces them.
func (s *GenAISession) Stream(ctx context.Context, prompt string, opts GenerateOptions) (<-chan StreamChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if prompt == "" {
		return nil, errors.New("openvino GenAI prompt is required")
	}

	s.mu.Lock()
	if s.ptr == nil {
		s.mu.Unlock()
		return nil, errors.New("openvino GenAI session is closed")
	}
	ptr := s.ptr

	stream := C.cx_genai_stream_new()
	if stream == nil {
		s.mu.Unlock()
		return nil, errors.New("allocate OpenVINO GenAI stream")
	}

	ch := make(chan StreamChunk, 16)
	generatorDone := make(chan struct{})

	go func() {
		defer close(generatorDone)
		defer s.mu.Unlock()

		cPrompt := C.CString(prompt)
		defer C.free(unsafe.Pointer(cPrompt))

		errbuf := C.calloc(1, C.size_t(genAIErrLen))
		if errbuf == nil {
			msg := C.CString("allocate OpenVINO GenAI stream generator error buffer")
			C.cx_genai_stream_abort(stream, msg)
			C.free(unsafe.Pointer(msg))
			C.cx_genai_session_cancel(ptr)
			return
		}
		defer C.free(errbuf)

		cParserProtocols := C.CString(strings.Join(opts.ParserProtocols, "\n"))
		defer C.free(unsafe.Pointer(cParserProtocols))

		var cmetrics C.cx_genai_metrics
		var temp C.float
		var useTemp C.int
		if opts.Temperature != nil {
			temp = C.float(*opts.Temperature)
			useTemp = 1
		}
		var topP C.float
		var useTopP C.int
		if opts.TopP != nil {
			topP = C.float(*opts.TopP)
			useTopP = 1
		}
		var topK C.size_t
		var useTopK C.int
		if opts.TopK != nil && *opts.TopK > 0 {
			topK = C.size_t(*opts.TopK)
			useTopK = 1
		}
		var seed C.size_t
		var useSeed C.int
		if opts.Seed != nil && *opts.Seed >= 0 {
			seed = C.size_t(*opts.Seed)
			useSeed = 1
		}

		done := make(chan struct{})
		if ctx.Done() != nil {
			go func() {
				select {
				case <-ctx.Done():
					C.cx_genai_session_cancel(ptr)
				case <-done:
				}
			}()
		}
		C.cx_genai_generate_stream(
			ptr,
			cPrompt,
			C.size_t(max(opts.MaxNewTokens, 0)),
			temp,
			useTemp,
			topP,
			useTopP,
			topK,
			useTopK,
			seed,
			useSeed,
			cParserProtocols,
			stream,
			&cmetrics,
			(*C.char)(errbuf),
			C.size_t(genAIErrLen),
		)
		close(done)
	}()

	go func() {
		defer close(ch)
		defer func() {
			<-generatorDone
			C.cx_genai_stream_free(stream)
		}()

		errbuf := C.calloc(1, C.size_t(genAIErrLen))
		if errbuf == nil {
			_ = sessionkit.Send(ctx, ch, StreamChunk{Error: errors.New("allocate OpenVINO GenAI stream error buffer")})
			C.cx_genai_session_cancel(ptr)
			return
		}
		defer C.free(errbuf)

		for {
			var out *C.char
			var outLen C.size_t
			var thinking *C.char
			var thinkingLen C.size_t
			rc := C.cx_genai_stream_next(
				stream,
				&out,
				&outLen,
				&thinking,
				&thinkingLen,
				(*C.char)(errbuf),
				C.size_t(genAIErrLen),
			)
			text := cAllocatedText(out, outLen)
			thinkingText := cAllocatedText(thinking, thinkingLen)
			freeCData(unsafe.Pointer(out))
			freeCData(unsafe.Pointer(thinking))
			switch rc {
			case 0:
				if text == "" && thinkingText == "" {
					continue
				}
				select {
				case ch <- StreamChunk{Text: text, Thinking: thinkingText}:
				case <-ctx.Done():
					C.cx_genai_session_cancel(ptr)
					sessionkit.TrySend(ch, StreamChunk{Error: ctx.Err()})
					return
				}
			case 1:
				if err := ctx.Err(); err != nil {
					sendTerminalStreamError(ctx, ch, err)
				}
				return
			case 3:
				if err := ctx.Err(); err != nil {
					sendTerminalStreamError(ctx, ch, err)
				} else {
					sendTerminalStreamError(ctx, ch, errors.New("openvino GenAI generation canceled"))
				}
				return
			default:
				sendTerminalStreamError(ctx, ch, fmt.Errorf("openvino GenAI stream: %s", C.GoString((*C.char)(errbuf))))
				return
			}
		}
	}()

	return ch, nil
}

// StreamTokens runs an already-tokenized prompt and returns decoded text deltas
// as GenAI produces them.
func (s *GenAISession) StreamTokens(ctx context.Context, tokens []int, opts GenerateOptions) (<-chan StreamChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, errors.New("openvino GenAI token prompt is empty")
	}
	cTokens := make([]C.int64_t, len(tokens))
	for i, tok := range tokens {
		cTokens[i] = C.int64_t(tok)
	}

	s.mu.Lock()
	if s.ptr == nil {
		s.mu.Unlock()
		return nil, errors.New("openvino GenAI session is closed")
	}
	ptr := s.ptr

	stream := C.cx_genai_stream_new()
	if stream == nil {
		s.mu.Unlock()
		return nil, errors.New("allocate OpenVINO GenAI stream")
	}

	ch := make(chan StreamChunk, 16)
	generatorDone := make(chan struct{})

	go func() {
		defer close(generatorDone)
		defer s.mu.Unlock()

		errbuf := C.calloc(1, C.size_t(genAIErrLen))
		if errbuf == nil {
			msg := C.CString("allocate OpenVINO GenAI stream generator error buffer")
			C.cx_genai_stream_abort(stream, msg)
			C.free(unsafe.Pointer(msg))
			C.cx_genai_session_cancel(ptr)
			return
		}
		defer C.free(errbuf)

		cParserProtocols := C.CString(strings.Join(opts.ParserProtocols, "\n"))
		defer C.free(unsafe.Pointer(cParserProtocols))

		var cmetrics C.cx_genai_metrics
		var temp C.float
		var useTemp C.int
		if opts.Temperature != nil {
			temp = C.float(*opts.Temperature)
			useTemp = 1
		}
		var topP C.float
		var useTopP C.int
		if opts.TopP != nil {
			topP = C.float(*opts.TopP)
			useTopP = 1
		}
		var topK C.size_t
		var useTopK C.int
		if opts.TopK != nil && *opts.TopK > 0 {
			topK = C.size_t(*opts.TopK)
			useTopK = 1
		}
		var seed C.size_t
		var useSeed C.int
		if opts.Seed != nil && *opts.Seed >= 0 {
			seed = C.size_t(*opts.Seed)
			useSeed = 1
		}

		done := make(chan struct{})
		if ctx.Done() != nil {
			go func() {
				select {
				case <-ctx.Done():
					C.cx_genai_session_cancel(ptr)
				case <-done:
				}
			}()
		}
		C.cx_genai_generate_tokens_stream(
			ptr,
			(*C.int64_t)(unsafe.Pointer(&cTokens[0])),
			C.size_t(len(cTokens)),
			C.size_t(max(opts.MaxNewTokens, 0)),
			temp,
			useTemp,
			topP,
			useTopP,
			topK,
			useTopK,
			seed,
			useSeed,
			cParserProtocols,
			stream,
			&cmetrics,
			(*C.char)(errbuf),
			C.size_t(genAIErrLen),
		)
		close(done)
	}()

	go func() {
		defer close(ch)
		defer func() {
			<-generatorDone
			C.cx_genai_stream_free(stream)
		}()

		errbuf := C.calloc(1, C.size_t(genAIErrLen))
		if errbuf == nil {
			_ = sessionkit.Send(ctx, ch, StreamChunk{Error: errors.New("allocate OpenVINO GenAI stream error buffer")})
			C.cx_genai_session_cancel(ptr)
			return
		}
		defer C.free(errbuf)

		for {
			var out *C.char
			var outLen C.size_t
			var thinking *C.char
			var thinkingLen C.size_t
			rc := C.cx_genai_stream_next(
				stream,
				&out,
				&outLen,
				&thinking,
				&thinkingLen,
				(*C.char)(errbuf),
				C.size_t(genAIErrLen),
			)
			text := cAllocatedText(out, outLen)
			thinkingText := cAllocatedText(thinking, thinkingLen)
			freeCData(unsafe.Pointer(out))
			freeCData(unsafe.Pointer(thinking))
			switch rc {
			case 0:
				if text == "" && thinkingText == "" {
					continue
				}
				select {
				case ch <- StreamChunk{Text: text, Thinking: thinkingText}:
				case <-ctx.Done():
					C.cx_genai_session_cancel(ptr)
					sessionkit.TrySend(ch, StreamChunk{Error: ctx.Err()})
					return
				}
			case 1:
				if err := ctx.Err(); err != nil {
					sendTerminalStreamError(ctx, ch, err)
				}
				return
			case 3:
				if err := ctx.Err(); err != nil {
					sendTerminalStreamError(ctx, ch, err)
				} else {
					sendTerminalStreamError(ctx, ch, errors.New("openvino GenAI generation canceled"))
				}
				return
			default:
				sendTerminalStreamError(ctx, ch, fmt.Errorf("openvino GenAI stream tokens: %s", C.GoString((*C.char)(errbuf))))
				return
			}
		}
	}()

	return ch, nil
}

// Close releases the native GenAI session.
func (s *GenAISession) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return nil
	}
	runtime.SetFinalizer(s, nil)
	C.cx_genai_session_free(s.ptr)
	s.ptr = nil
	return nil
}

func (s *GenAISession) mustClose() {
	_ = s.Close()
}

// ChatMessage is one role/content turn for chat-template rendering.
type ChatMessage struct {
	Role       string
	Content    string
	ToolCalls  string // raw JSON array, e.g. [{"id":"...","type":"function",...}]
	ToolCallID string // for role=="tool" result turns
}

// ApplyChatTemplate renders messages with the model's own chat template
// (from tokenizer_config.json) via OpenVINO, returning the prompt string to feed
// to Generate/Stream. This replaces hand-rolled prompt formatting so the model
// sees the format it was trained on.
func (s *GenAISession) ApplyChatTemplate(messages []ChatMessage, toolsJSON string) (string, error) {
	return s.ApplyChatTemplateWithPrompt(messages, toolsJSON, true)
}

// ApplyChatTemplateWithPrompt renders messages with explicit control over
// whether the model's assistant generation cue should be appended.
func (s *GenAISession) ApplyChatTemplateWithPrompt(messages []ChatMessage, toolsJSON string, addGenerationPrompt bool) (string, error) {
	return s.ApplyChatTemplateWithOptions(messages, toolsJSON, addGenerationPrompt, nil, "")
}

// ApplyChatTemplateWithOptions additionally forwards thinking/effort controls
// as chat-template extra context (enable_thinking / reasoning_effort
// variables). nil / empty means backend default: no extra context is sent.
func (s *GenAISession) ApplyChatTemplateWithOptions(messages []ChatMessage, toolsJSON string, addGenerationPrompt bool, enableThinking *bool, reasoningEffort string) (string, error) {
	if len(messages) == 0 {
		return "", errors.New("openvino GenAI chat template requires at least one message")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return "", errors.New("openvino GenAI session is closed")
	}

	roles := make([]*C.char, len(messages))
	contents := make([]*C.char, len(messages))
	toolCalls := make([]*C.char, len(messages))
	toolCallIDs := make([]*C.char, len(messages))
	for i, m := range messages {
		roles[i] = C.CString(m.Role)
		contents[i] = C.CString(m.Content)
		if m.ToolCalls != "" {
			toolCalls[i] = C.CString(m.ToolCalls)
		}
		if m.ToolCallID != "" {
			toolCallIDs[i] = C.CString(m.ToolCallID)
		}
	}
	defer func() {
		for i := range messages {
			C.free(unsafe.Pointer(roles[i]))
			C.free(unsafe.Pointer(contents[i]))
			if toolCalls[i] != nil {
				C.free(unsafe.Pointer(toolCalls[i]))
			}
			if toolCallIDs[i] != nil {
				C.free(unsafe.Pointer(toolCallIDs[i]))
			}
		}
	}()

	var cTools *C.char
	if toolsJSON != "" {
		cTools = C.CString(toolsJSON)
		defer C.free(unsafe.Pointer(cTools))
	}

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return "", errors.New("allocate OpenVINO GenAI error buffer")
	}
	defer C.free(errbuf)

	cEnableThinking := C.int(-1)
	if enableThinking != nil {
		if *enableThinking {
			cEnableThinking = 1
		} else {
			cEnableThinking = 0
		}
	}
	var cReasoningEffort *C.char
	if reasoningEffort != "" {
		cReasoningEffort = C.CString(reasoningEffort)
		defer C.free(unsafe.Pointer(cReasoningEffort))
	}

	var out *C.char
	var outLen C.size_t
	rc := C.cx_genai_apply_chat_template(
		s.ptr,
		(**C.char)(unsafe.Pointer(&roles[0])),
		(**C.char)(unsafe.Pointer(&contents[0])),
		(**C.char)(unsafe.Pointer(&toolCalls[0])),
		(**C.char)(unsafe.Pointer(&toolCallIDs[0])),
		C.size_t(len(messages)),
		cTools,
		cbool(addGenerationPrompt),
		cEnableThinking,
		cReasoningEffort,
		&out,
		&outLen,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	defer freeCData(unsafe.Pointer(out))
	if rc != 0 {
		return "", fmt.Errorf("openvino GenAI apply chat template: %s", C.GoString((*C.char)(errbuf)))
	}
	return cAllocatedText(out, outLen), nil
}

// Tokenize encodes prompt text with the model tokenizer owned by the GenAI
// session. It is an observability/correctness primitive for manifest token
// hashes; generation still receives rendered prompt text.
func (s *GenAISession) Tokenize(ctx context.Context, prompt string, addSpecial bool) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if prompt == "" {
		return nil, errors.New("openvino GenAI tokenization prompt is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return nil, errors.New("openvino GenAI session is closed")
	}

	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return nil, errors.New("allocate OpenVINO GenAI tokenization error buffer")
	}
	defer C.free(errbuf)

	var required C.size_t
	rc := C.cx_genai_tokenize(
		s.ptr,
		cPrompt,
		cbool(addSpecial),
		nil,
		0,
		&required,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	if rc != 2 && rc != 0 {
		return nil, fmt.Errorf("openvino GenAI tokenize: %s", C.GoString((*C.char)(errbuf)))
	}
	if required == 0 {
		return nil, nil
	}

	buf := make([]C.int64_t, int(required))
	rc = C.cx_genai_tokenize(
		s.ptr,
		cPrompt,
		cbool(addSpecial),
		(*C.int64_t)(unsafe.Pointer(&buf[0])),
		required,
		&required,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	if rc != 0 {
		return nil, fmt.Errorf("openvino GenAI tokenize: %s", C.GoString((*C.char)(errbuf)))
	}
	out := make([]int, len(buf))
	for i, tok := range buf {
		out[i] = int(tok)
	}
	return out, nil
}

func normalizeGenAIConfig(cfg GenAIConfig) GenAIConfig {
	if cfg.KVCachePrecision == "" {
		cfg.KVCachePrecision = "f16"
	}
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = 1
	}
	if cfg.NumLastDenseTokensInPrefill <= 0 {
		cfg.NumLastDenseTokensInPrefill = 10
	}
	if cfg.XAttentionThreshold <= 0 {
		cfg.XAttentionThreshold = 0.9
	}
	if cfg.XAttentionBlockSize <= 0 {
		cfg.XAttentionBlockSize = 128
	}
	if cfg.XAttentionStride <= 0 {
		cfg.XAttentionStride = 16
	}
	return cfg
}

func boolValue(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func cbool(v bool) C.int {
	if v {
		return 1
	}
	return 0
}
